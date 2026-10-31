package main

// /agent-pin (Autopilot M4's agent release, decided 2026-10-31): open the pull request that pins an agent definition,
// for the app whose pipeline asks. The pin is an Airframe Agent XR in the control-plane tenants repo
// (tenants/<app>/xr-requests/<name>.yaml); merging it is what makes Clearance load that commit of the file.
//
// The broker writes the PR itself and hands nothing back but its number: no app pipeline ever holds a token for the
// tenants repo (every app's requests live there). What a caller may ask for, all checked here:
//
//   - The caller is an app's own -cicd namespace (TokenReview, audience agent-pin), and the source repo is that app's
//     own, by /github-installation-token's rule (its Repository CR).
//   - The pin lands under the caller's own app only: tenants/<app>/xr-requests/<name>.yaml, the app taken from the
//     namespace, never from the request.
//   - name is a DNS label, path agents/<name>.yaml, commit 40 hex, owner a group ref. The file is generated here.
//   - The token is the platform App's on the tenants repo only, contents and pull_requests write.
//
// AGENT_PIN_TENANTS_REPO (<owner>/<repo>) configures it; unset, the endpoint answers 503.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/glidepath/broker/token-review-interceptor/internal/githubapp"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const agentPinAudience = "agent-pin"

var (
	pinName      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	pinCommit    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	pinOwner     = regexp.MustCompile(`^group:[a-z0-9-]+/[a-z0-9._-]+$`)
	pinNamespace = regexp.MustCompile(`^(app|infra)-([a-z0-9][a-z0-9-]*)-cicd$`)
)

type agentPinRequest struct {
	Owner       string `json:"owner"`       // the source repo's GitHub owner
	Repo        string `json:"repo"`        // the source repo's name
	Name        string `json:"name"`        // the agent
	Path        string `json:"path"`        // agents/<name>.yaml
	Commit      string `json:"commit"`      // the commit to pin
	Group       string `json:"group"`       // the agent's owner group (group:default/<team>)
	Description string `json:"description"` // one line, optional
	Remove      bool   `json:"remove"`      // the definition was deleted: remove the pin
	Body        string `json:"body"`        // the PR description the caller writes (what changed), optional
}

type agentPinResponse struct {
	PR     int    `json:"pr,omitempty"`
	URL    string `json:"url,omitempty"`
	Head   string `json:"head,omitempty"`
	Branch string `json:"branch,omitempty"`
	File   string `json:"file,omitempty"`
	Error  string `json:"error,omitempty"`
}

// appOfNamespace is the app a -cicd namespace belongs to.
func appOfNamespace(ns string) (string, bool) {
	m := pinNamespace.FindStringSubmatch(ns)
	if m == nil {
		return "", false
	}
	return m[2], true
}

// checkPinRequest is the whole request decision except repo ownership, pure for a table test.
func checkPinRequest(req agentPinRequest) error {
	switch {
	case !ownerPattern.MatchString(req.Owner) || !pinName.MatchString(req.Repo):
		return fmt.Errorf("owner/repo %q/%q is not a GitHub repo name", req.Owner, req.Repo)
	case !pinName.MatchString(req.Name):
		return fmt.Errorf("name %q is not a DNS label", req.Name)
	case req.Path != "agents/"+req.Name+".yaml" && req.Path != "agents/"+req.Name+".yml":
		return fmt.Errorf("path must be agents/%s.yaml, not %q", req.Name, req.Path)
	case !pinCommit.MatchString(req.Commit):
		return fmt.Errorf("commit must be a full 40-hex SHA")
	case !req.Remove && !pinOwner.MatchString(req.Group):
		return fmt.Errorf("group %q is not a group ref (group:default/<team>)", req.Group)
	case len(req.Description) > 200 || strings.ContainsAny(req.Description, "\n\r"):
		return fmt.Errorf("description is one line of at most 200 characters")
	case len(req.Body) > 60000:
		return fmt.Errorf("body is too long")
	}
	return nil
}

// pinFile is the Agent XR the PR writes (airframe xrds/agent.yaml).
func pinFile(app string, req agentPinRequest) string {
	desc := ""
	if req.Description != "" {
		b, _ := json.Marshal(req.Description)
		desc = "  description: " + string(b) + "\n"
	}
	return fmt.Sprintf(`# tenants/%[1]s/xr-requests/%[2]s.yaml: the Agent pin (airframe xrds/agent.yaml), opened by Glidepath's agent
# release for %[3]s/%[4]s at %[5]s. Merging it makes that commit of %[6]s live: Clearance loads it within a minute.
apiVersion: catalog.hangar.io/v1alpha1
kind: Agent
metadata:
  name: %[2]s
  namespace: app-%[1]s-cicd
spec:
  app: %[1]s
  owner: %[7]s
%[8]s  source:
    repo: %[3]s/%[4]s
    path: %[6]s
    commit: %[5]s
`, app, req.Name, req.Owner, req.Repo, req.Commit, req.Path, req.Group, desc)
}

type ghClient struct{ token string }

func (g ghClient) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, githubapp.APIBase()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, out, nil
}

// openPinPR writes the file (or removes it) on a new branch and opens the PR; an open PR on the same branch is
// updated, not duplicated.
func openPinPR(g ghClient, tenants, app string, req agentPinRequest) (agentPinResponse, error) {
	file := fmt.Sprintf("tenants/%s/xr-requests/%s.yaml", app, req.Name)
	branch := fmt.Sprintf("agent-release/%s-%s-%s", app, req.Name, req.Commit[:7])
	code, b, err := g.do("GET", "/repos/"+tenants+"/git/ref/heads/main", nil)
	if err != nil || code != 200 {
		return agentPinResponse{}, fmt.Errorf("reading %s main: %d %s", tenants, code, b)
	}
	var ref struct{ Object struct{ SHA string } }
	_ = json.Unmarshal(b, &ref)
	code, _, err = g.do("POST", "/repos/"+tenants+"/git/refs", map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA})
	if err != nil || (code != 201 && code != 422) { // 422: the branch exists (a re-run): keep going
		return agentPinResponse{}, fmt.Errorf("creating branch %s: %d", branch, code)
	}
	var existing struct{ SHA string }
	if code, b, _ = g.do("GET", "/repos/"+tenants+"/contents/"+file+"?ref="+branch, nil); code == 200 {
		_ = json.Unmarshal(b, &existing)
	}
	msg := fmt.Sprintf("agent pin: %s (%s) @ %s", req.Name, app, req.Commit[:7])
	if req.Remove {
		if existing.SHA == "" {
			return agentPinResponse{}, fmt.Errorf("no pin %s to remove", file)
		}
		msg = fmt.Sprintf("agent pin: remove %s (%s)", req.Name, app)
		code, b, err = g.do("DELETE", "/repos/"+tenants+"/contents/"+file, map[string]string{"message": msg, "sha": existing.SHA, "branch": branch})
	} else {
		put := map[string]string{"message": msg, "branch": branch, "content": base64.StdEncoding.EncodeToString([]byte(pinFile(app, req)))}
		if existing.SHA != "" {
			put["sha"] = existing.SHA
		}
		code, b, err = g.do("PUT", "/repos/"+tenants+"/contents/"+file, put)
	}
	if err != nil || (code != 200 && code != 201) {
		return agentPinResponse{}, fmt.Errorf("writing %s: %d %s", file, code, b)
	}
	var wrote struct{ Commit struct{ SHA string } }
	_ = json.Unmarshal(b, &wrote)
	body := req.Body
	if body == "" {
		body = fmt.Sprintf("Pins `%s` to `%s/%s` at `%s`. Merging this makes that commit live.", req.Name, req.Owner, req.Repo, req.Commit)
	}
	code, b, _ = g.do("POST", "/repos/"+tenants+"/pulls", map[string]string{"title": msg, "head": branch, "base": "main", "body": body})
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if code == 201 {
		_ = json.Unmarshal(b, &pr)
	} else {
		owner := strings.SplitN(tenants, "/", 2)[0]
		c2, b2, _ := g.do("GET", "/repos/"+tenants+"/pulls?state=open&head="+owner+":"+branch, nil)
		var open []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		}
		if c2 != 200 || json.Unmarshal(b2, &open) != nil || len(open) == 0 {
			return agentPinResponse{}, fmt.Errorf("opening the PR: %d %s", code, b)
		}
		pr.Number, pr.HTMLURL = open[0].Number, open[0].HTMLURL
		_, _, _ = g.do("PATCH", fmt.Sprintf("/repos/%s/pulls/%d", tenants, pr.Number), map[string]string{"body": body})
	}
	return agentPinResponse{PR: pr.Number, URL: pr.HTMLURL, Head: wrote.Commit.SHA, Branch: branch, File: file}, nil
}

func handleAgentPin(clientset kubernetes.Interface, dynClient dynamic.Interface, appCreds *githubapp.Creds) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(agentPinResponse{Error: msg})
		}
		tenants := os.Getenv("AGENT_PIN_TENANTS_REPO")
		if o, rp, ok := strings.Cut(tenants, "/"); !ok || !ownerPattern.MatchString(o) || rp == "" {
			fail(http.StatusServiceUnavailable, "agent pins are not configured (AGENT_PIN_TENANTS_REPO unset)")
			return
		}
		if r.Method != http.MethodPost {
			fail(http.StatusMethodNotAllowed, "POST only")
			return
		}
		token := bearerToken(r.Header.Get("Authorization"))
		if token == "" {
			fail(http.StatusUnauthorized, "missing or malformed Authorization: Bearer <token> header")
			return
		}
		ns, authErr := verifyCallerAppNamespace(clientset, token, agentPinAudience)
		if authErr != "" {
			fail(http.StatusUnauthorized, authErr)
			return
		}
		app, ok := appOfNamespace(ns)
		if !ok {
			fail(http.StatusForbidden, fmt.Sprintf("namespace %q is not an app's -cicd namespace", ns))
			return
		}
		var req agentPinRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 128<<10)).Decode(&req); err != nil {
			fail(http.StatusBadRequest, "the body is not JSON")
			return
		}
		if err := checkPinRequest(req); err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		if err := verifyAppOwnsRepo(dynClient, ns, req.Repo); err != nil {
			fail(http.StatusForbidden, err.Error())
			return
		}
		appJWT, err := appCreds.SignAppJWT()
		if err != nil {
			fail(http.StatusInternalServerError, "signing App JWT: "+err.Error())
			return
		}
		o, rp, _ := strings.Cut(tenants, "/")
		inst, err := githubapp.InstallationIDForRepo(appJWT, o, rp)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		tok, _, err := githubapp.MintScopedToken(appJWT, inst, rp, map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"})
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		out, err := openPinPR(ghClient{token: tok}, tenants, app, req)
		if err != nil {
			fail(http.StatusBadGateway, err.Error())
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}
}
