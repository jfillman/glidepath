package changekind

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/glidepath/api/internal/apps"
)

// Bounds a computation never exceeds; past one the answer is image-release (fail closed).
const (
	// GitHub's compare lists at most 300 files: a list that long may be cut short, so it cannot show that nothing else changed.
	compareFileLimit = 300
	// Two content reads per deployable file.
	maxDeployableFiles = 100
	// A pull request's commits list stops at 250 (pulls/{n}/commits).
	maxCommitPages = 3
	maxPullCommits = 250
	maxChanged     = 10 // configuration paths listed per environment
	maxReasonEnvs  = 3  // environments named in the pull request's reason
	maxReasonRefs  = 3  // new image references named in an environment's reason
)

// The deployable-files predicate of the gate templates (glidepath#299; spec section 4), as unanchored RE2 matches like CEL's
// matches(): a cheap superset. Anything it admits is judged on its content.
var (
	deployableName = regexp.MustCompile(`(values|release|base|kustomization)[^/]*\.ya?ml$`)
	deployableEnv  = regexp.MustCompile(`^[^/]+/[^/]+/.+\.(ya?ml|json)$`)
	// <cluster>/<env>/(release|values).yaml: the environment's release file (extract-promoted-image's rollback-eligibility).
	releaseFileRE = regexp.MustCompile(`^([^/]+)/([^/]+)/(release|values)\.yaml$`)
)

// Deployable reports whether a changed path is one the classifier judges: the gate templates' predicate, plus the files of an
// own chart under <cluster>/<env>/ (spec section 5.2, step 2), which the predicate misses because it matches only YAML and JSON.
func Deployable(f string) bool {
	if strings.HasPrefix(f, ".tekton/") || strings.HasPrefix(f, "releases/") {
		return false
	}
	return deployableName.MatchString(f) || strings.HasSuffix(f, "Kustomization") || deployableEnv.MatchString(f) || ownChartFile(f)
}

// ownChartFile: a template-side file of a Helm chart kept under <cluster>/<env>/ (a .tpl helper, anything under templates/,
// Chart.yaml, Chart.lock, a packaged subchart under charts/). It is not parsed (a template is not YAML); its environment is config
// with a render check. A chart's values files (any other YAML or JSON) are parsed like every values file, so an image change in
// them is an image release.
func ownChartFile(f string) bool {
	segs := strings.Split(f, "/")
	if len(segs) < 3 || strings.HasPrefix(f, ".tekton/") || strings.HasPrefix(f, "releases/") {
		return false
	}
	base := segs[len(segs)-1]
	if strings.HasSuffix(base, ".tpl") || base == "Chart.yaml" || base == "Chart.lock" {
		return true
	}
	data := strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".json")
	for _, s := range segs[2 : len(segs)-1] {
		if s == "templates" || (s == "charts" && !data) {
			return true
		}
	}
	return false
}

func kustomizationFile(f string) bool {
	b := path.Base(f)
	return strings.HasPrefix(b, "kustomization") || b == "Kustomization"
}

type ghPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Base    struct {
		Sha string `json:"sha"`
	} `json:"base"`
	Head struct {
		Sha string `json:"sha"`
	} `json:"head"`
}

type ghFile struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	PreviousFilename string `json:"previous_filename"`
}

// doubt is a reason to fail closed. transient: a read failed, so the answer is not stored and a later ask computes again.
type doubt struct {
	why       string
	transient bool
}

func (d *doubt) Error() string { return d.why }

func readFailed(what string, status int, err error) *doubt {
	if err != nil {
		return &doubt{fmt.Sprintf("could not read %s: %v", what, err), true}
	}
	return &doubt{fmt.Sprintf("could not read %s: GitHub answered %d", what, status), true}
}

// compute classifies pull request n at head. A doubt is a fail-closed classification, not an error; transient says it must not
// be stored. The errors are the request's own: no such pull request, a head that is not one of its commits.
func (s *Service) compute(ctx context.Context, app *apps.App, n int, head string) (*Classification, bool, error) {
	owner, repo, err := s.repo(app)
	if err != nil {
		return nil, false, err
	}
	c := &Classification{V: Version, Classifier: Classifier, Repo: owner + "/" + repo, PR: n, Head: head, RenderCheck: RenderNone,
		ComputedAt: s.now().UTC().Format(time.RFC3339), Envs: []Env{}}
	j := &job{s: s, rd: s.GitHub(app.Name), owner: owner, repo: repo, app: app, c: c}
	err = j.run(ctx)
	var d *doubt
	switch {
	case err == nil:
		return c, false, nil
	case asDoubt(err, &d):
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		c.Kind, c.FailClosed, c.Reason, c.Envs, c.RenderCheck = ImageRelease, true, "fail closed: "+d.why, []Env{}, RenderNone
		return c, d.transient, nil
	default:
		return nil, false, err
	}
}

func asDoubt(err error, d **doubt) bool {
	x, ok := err.(*doubt)
	if ok {
		*d = x
	}
	return ok
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type job struct {
	s           *Service
	rd          Reader
	owner, repo string
	app         *apps.App
	c           *Classification
	clusterEnvs map[string][]string
}

func (j *job) get(ctx context.Context, p string, out any) (int, error) {
	status, body, err := j.rd.GetJSON(ctx, j.owner, j.repo, p)
	if err != nil {
		return status, err
	}
	if status == http.StatusOK && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return status, &doubt{fmt.Sprintf("GitHub's answer to %s does not parse (it may be cut short): %v", p, err), false}
		}
	}
	return status, nil
}

func (j *job) run(ctx context.Context) error {
	var pr ghPull
	switch st, err := j.get(ctx, fmt.Sprintf("pulls/%d", j.c.PR), &pr); {
	case err != nil:
		if _, ok := err.(*doubt); ok {
			return err
		}
		return readFailed(fmt.Sprintf("pull request #%d", j.c.PR), st, err)
	case st == http.StatusNotFound:
		return NotFound{fmt.Sprintf("no pull request #%d in %s/%s", j.c.PR, j.owner, j.repo)}
	case st != http.StatusOK:
		return readFailed(fmt.Sprintf("pull request #%d", j.c.PR), st, nil)
	}
	if !shaRE.MatchString(pr.Base.Sha) {
		return &doubt{fmt.Sprintf("pull request #%d has no base commit", j.c.PR), true}
	}
	j.c.Base, j.c.PrURL = pr.Base.Sha, pr.HTMLURL
	if pr.Head.Sha != j.c.Head {
		if err := j.inPull(ctx); err != nil {
			return err
		}
	}
	var cmp struct {
		Files           []ghFile `json:"files"`
		MergeBaseCommit struct {
			Sha string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	st, err := j.get(ctx, fmt.Sprintf("compare/%s...%s", j.c.Base, j.c.Head), &cmp)
	if err != nil {
		if _, ok := err.(*doubt); ok {
			return err
		}
		return readFailed("the comparison", st, err)
	}
	if st != http.StatusOK {
		return readFailed("the comparison", st, nil)
	}
	// A three-dot comparison lists what changed since the merge base, not since the base branch's tip: the base side of every
	// file is read there, or a change the base branch has since made too would look like no change at all.
	if !shaRE.MatchString(cmp.MergeBaseCommit.Sha) {
		return &doubt{"GitHub's comparison names no merge base", true}
	}
	j.c.Base = cmp.MergeBaseCommit.Sha
	if len(cmp.Files) >= compareFileLimit {
		return &doubt{fmt.Sprintf("the pull request changes %d or more files, and GitHub lists at most %d: it cannot show that nothing else changed", len(cmp.Files), compareFileLimit), false}
	}
	return j.classify(ctx, cmp.Files)
}

// inPull: head must be one of the pull request's commits (an earlier head a gate was started for).
func (j *job) inPull(ctx context.Context) error {
	seen := 0
	for page := 1; page <= maxCommitPages; page++ {
		var commits []struct {
			Sha string `json:"sha"`
		}
		st, err := j.get(ctx, fmt.Sprintf("pulls/%d/commits?per_page=100&page=%d", j.c.PR, page), &commits)
		if err != nil {
			if _, ok := err.(*doubt); ok {
				return err
			}
			return readFailed(fmt.Sprintf("the commits of pull request #%d", j.c.PR), st, err)
		}
		if st != http.StatusOK {
			return readFailed(fmt.Sprintf("the commits of pull request #%d", j.c.PR), st, nil)
		}
		for _, c := range commits {
			if c.Sha == j.c.Head {
				return nil
			}
		}
		seen += len(commits)
		if len(commits) < 100 {
			break
		}
	}
	if seen >= maxPullCommits {
		// GitHub lists at most 250 commits of a pull request: past that, not finding it proves nothing.
		return &doubt{fmt.Sprintf("pull request #%d has %d or more commits, and GitHub lists at most %d: %s could not be found among them", j.c.PR, seen, maxPullCommits, j.c.Head[:12]), true}
	}
	return Refused{fmt.Sprintf("%s is not a commit of pull request #%d", j.c.Head[:12], j.c.PR),
		"ask for the head the gate run was started for; a commit force-pushed away is no longer the pull request's"}
}

// envKey names the environment a file feeds.
type envKey struct{ cluster, env string }

func (k envKey) String() string {
	switch {
	case k.cluster == "*":
		return "(repository root)"
	default:
		return k.cluster + "/" + k.env
	}
}

// fileChange is one deployable file, read at base and head.
type fileChange struct {
	gh         ghFile
	basePath   string // "" when the file is new
	headPath   string // "" when it was removed
	base, head *parsed
	ownChart   bool
	rawEqual   bool
}

type envAcc struct {
	key          envKey
	sides        []side
	headRefs     []located
	headRefSet   map[string]bool
	baseRefSet   map[string]bool
	configChange []string
	renderCheck  bool
}

type located struct {
	file string
	ref  imageRef
}

func (j *job) classify(ctx context.Context, files []ghFile) error {
	var changes []*fileChange
	var skipped int
	for _, f := range files {
		if !Deployable(f.Filename) && (f.PreviousFilename == "" || !Deployable(f.PreviousFilename)) {
			skipped++
			continue
		}
		fc := &fileChange{gh: f, ownChart: ownChartFile(f.Filename) || (f.PreviousFilename != "" && ownChartFile(f.PreviousFilename))}
		switch f.Status {
		case "added":
			fc.headPath = f.Filename
		case "removed":
			fc.basePath = f.Filename
		case "renamed", "copied":
			fc.headPath, fc.basePath = f.Filename, f.PreviousFilename
			if f.Status == "copied" {
				fc.basePath = ""
			}
		default: // modified, changed (mode), unchanged
			fc.headPath, fc.basePath = f.Filename, f.Filename
		}
		changes = append(changes, fc)
	}
	if len(changes) == 0 {
		j.c.Kind = NoOp
		j.c.Reason = fmt.Sprintf("no deployable file changed (%d file%s outside the environments: .tekton/, releases/, docs)", skipped, plural(skipped))
		return nil
	}
	if len(changes) > maxDeployableFiles {
		return &doubt{fmt.Sprintf("the pull request changes %d deployable files; the classifier reads at most %d", len(changes), maxDeployableFiles), false}
	}
	envs := map[envKey]*envAcc{}
	for _, fc := range changes {
		if err := j.read(ctx, fc); err != nil {
			return err
		}
		baseEnvs, err := j.envsOf(ctx, fc.basePath)
		if err != nil {
			return err
		}
		headEnvs, err := j.envsOf(ctx, fc.headPath)
		if err != nil {
			return err
		}
		all := map[envKey]bool{}
		for k := range baseEnvs {
			all[k] = true
		}
		for k := range headEnvs {
			all[k] = true
		}
		for k := range all {
			e := envs[k]
			if e == nil {
				e = &envAcc{key: k, headRefSet: map[string]bool{}, baseRefSet: map[string]bool{}}
				envs[k] = e
			}
			e.sides = append(e.sides, side{fc: fc, inBase: baseEnvs[k], inHead: headEnvs[k]})
		}
	}
	keys := make([]envKey, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a].String() < keys[b].String() })
	rank := map[string]int{ImageRelease: 1, Rollback: 2, Config: 3, NoOp: 4}
	j.c.Kind = NoOp
	byKind := map[string][]string{}
	for _, k := range keys {
		env, err := j.judge(ctx, envs[k])
		if err != nil {
			return err
		}
		j.c.Envs = append(j.c.Envs, env)
		if rank[env.Kind] < rank[j.c.Kind] {
			j.c.Kind = env.Kind
		}
		if env.RenderCheck == RenderRequired {
			j.c.RenderCheck = RenderRequired
		}
		byKind[env.Kind] = append(byKind[env.Kind], env.Reason)
	}
	rs := byKind[j.c.Kind]
	if len(rs) > maxReasonEnvs {
		rs = append(rs[:maxReasonEnvs:maxReasonEnvs], fmt.Sprintf("and %d more", len(byKind[j.c.Kind])-maxReasonEnvs))
	}
	j.c.Reason = strings.Join(rs, "; ")
	return nil
}

// read fetches and parses the file at base and head.
func (j *job) read(ctx context.Context, fc *fileChange) error {
	var baseRaw, headRaw []byte
	var err error
	if fc.basePath != "" {
		if baseRaw, err = j.content(ctx, fc.basePath, j.c.Base); err != nil {
			return err
		}
	}
	if fc.headPath != "" {
		if headRaw, err = j.content(ctx, fc.headPath, j.c.Head); err != nil {
			return err
		}
	}
	fc.rawEqual = fc.basePath != "" && fc.headPath != "" && string(baseRaw) == string(headRaw)
	if fc.ownChart {
		return nil
	}
	if fc.basePath != "" {
		if fc.base, err = parse(fc.basePath, baseRaw); err != nil {
			return &doubt{fmt.Sprintf("%s does not parse at the base (%s): %v", fc.basePath, j.c.Base[:12], err), false}
		}
	}
	if fc.headPath != "" {
		if fc.head, err = parse(fc.headPath, headRaw); err != nil {
			return &doubt{fmt.Sprintf("%s does not parse at the head (%s): %v", fc.headPath, j.c.Head[:12], err), false}
		}
	}
	return nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, x := range parts {
		parts[i] = url.PathEscape(x)
	}
	return strings.Join(parts, "/")
}

// content is a file's bytes at ref (the contents API, base64 up to 1 MB; a larger file is a doubt).
func (j *job) content(ctx context.Context, p, ref string) ([]byte, error) {
	var body struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	st, err := j.get(ctx, "contents/"+escapePath(p)+"?ref="+url.QueryEscape(ref), &body)
	if err != nil {
		if _, ok := err.(*doubt); ok {
			return nil, err
		}
		return nil, readFailed(p+" at "+ref[:12], st, err)
	}
	if st != http.StatusOK {
		return nil, readFailed(p+" at "+ref[:12], st, nil)
	}
	if body.Type != "file" {
		return nil, &doubt{fmt.Sprintf("%s at %s is a %s, not a file", p, ref[:12], body.Type), false}
	}
	if body.Encoding != "base64" {
		return nil, &doubt{fmt.Sprintf("%s at %s is too large to read through the contents API (encoding %q)", p, ref[:12], body.Encoding), false}
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
	if err != nil {
		return nil, &doubt{fmt.Sprintf("%s at %s: %v", p, ref[:12], err), false}
	}
	return raw, nil
}

// envsOf maps a path to the environments it feeds: <cluster>/<env>/... is one; <cluster>/<file> feeds every environment directory
// under <cluster>/ (at base or head); a file at the root feeds the whole repository.
func (j *job) envsOf(ctx context.Context, p string) (map[envKey]bool, error) {
	out := map[envKey]bool{}
	if p == "" {
		return out, nil
	}
	segs := strings.Split(p, "/")
	switch {
	case len(segs) >= 3:
		out[envKey{segs[0], segs[1]}] = true
	case len(segs) == 2:
		names, err := j.envDirs(ctx, segs[0])
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			out[envKey{segs[0], n}] = true
		}
		if len(out) == 0 {
			out[envKey{segs[0], "*"}] = true
		}
	default:
		out[envKey{"*", "*"}] = true
	}
	return out, nil
}

// envDirs lists the environment directories of a cluster at head and at base.
func (j *job) envDirs(ctx context.Context, cluster string) ([]string, error) {
	if j.clusterEnvs == nil {
		j.clusterEnvs = map[string][]string{}
	}
	if v, ok := j.clusterEnvs[cluster]; ok {
		return v, nil
	}
	set := map[string]bool{}
	for _, ref := range []string{j.c.Head, j.c.Base} {
		var entries []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		st, err := j.get(ctx, "contents/"+escapePath(cluster)+"?ref="+url.QueryEscape(ref), &entries)
		if err != nil {
			if _, ok := err.(*doubt); ok {
				return nil, err
			}
			return nil, readFailed("the environments of "+cluster, st, err)
		}
		if st == http.StatusNotFound {
			continue
		}
		if st != http.StatusOK {
			return nil, readFailed("the environments of "+cluster, st, nil)
		}
		for _, e := range entries {
			if e.Type == "dir" {
				set[e.Name] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	j.clusterEnvs[cluster] = out
	return out, nil
}

// side is one file's contribution to one environment: its base side when the base path feeds the environment, its head side
// when the head path does. A file moved from one environment to another is a removal from the first and an addition to the second.
type side struct {
	fc             *fileChange
	inBase, inHead bool
}

func (sd side) baseDocs() []any {
	if sd.inBase && sd.fc.base != nil {
		return sd.fc.base.docs
	}
	return nil
}

func (sd side) headDocs() []any {
	if sd.inHead && sd.fc.head != nil {
		return sd.fc.head.docs
	}
	return nil
}

// judge decides one environment's kind.
func (j *job) judge(ctx context.Context, e *envAcc) (Env, error) {
	env := Env{Cluster: e.key.cluster, Env: e.key.env, RenderCheck: RenderNone, Images: ImageSets{Base: []string{}, Head: []string{}}}
	baseAt, headAt := map[string]bool{}, map[string]bool{} // "<file>#<doc>:<yaml path>=<ref>": where each reference is
	var removed []string
	for _, sd := range e.sides {
		fc := sd.fc
		env.Files = append(env.Files, displayName(fc))
		if sd.inBase && fc.base != nil {
			for _, r := range fc.base.refs {
				e.baseRefSet[r.Ref] = true
				baseAt[fmt.Sprintf("%s#%d:%s=%s", fc.basePath, r.Doc, r.Path, r.Ref)] = true
			}
		}
		if sd.inHead && fc.head != nil {
			for _, r := range fc.head.refs {
				e.headRefs = append(e.headRefs, located{fc.headPath, r})
				e.headRefSet[r.Ref] = true
				headAt[fmt.Sprintf("%s#%d:%s=%s", fc.headPath, r.Doc, r.Path, r.Ref)] = true
			}
		}
		e.configChange = append(e.configChange, configChange(sd)...)
		e.renderCheck = e.renderCheck || renderChanged(sd)
	}
	// An image reference removed or moved (no new one) is configuration: the environment changes what it runs. Named when nothing
	// else explains the change (a removed file already says so).
	for k := range baseAt {
		if !headAt[k] {
			_, ref, _ := strings.Cut(k, "=")
			removed = append(removed, ref)
		}
	}
	sort.Strings(removed)
	if len(removed) > 0 && len(e.configChange) == 0 {
		e.configChange = append(e.configChange, "image reference removed or moved: "+strings.Join(removed, ", "))
	}
	env.Images.Base, env.Images.Head = sortedKeys(e.baseRefSet), sortedKeys(e.headRefSet)
	var fresh []located // image references at head that the base does not hold: an image new to the environment
	for _, l := range e.headRefs {
		if !e.baseRefSet[l.ref.Ref] {
			fresh = append(fresh, l)
		}
	}
	if len(e.configChange) > maxChanged {
		env.Changed = append(append([]string{}, e.configChange[:maxChanged]...), fmt.Sprintf("and %d more", len(e.configChange)-maxChanged))
	} else {
		env.Changed = e.configChange
	}
	if e.renderCheck {
		env.RenderCheck = RenderRequired
	}
	where := e.key.String()
	switch {
	case len(fresh) > 0:
		env.Kind = ImageRelease
		var names []string
		for i, l := range fresh {
			if i == maxReasonRefs {
				names = append(names, fmt.Sprintf("and %d more", len(fresh)-maxReasonRefs))
				break
			}
			names = append(names, fmt.Sprintf("%s %s = %s", l.file, l.ref.Path, l.ref.Ref))
		}
		env.Reason = fmt.Sprintf("%s: new image reference (%s)", where, strings.Join(names, ", "))
		rb, err := j.rollback(ctx, e, fresh)
		if err != nil {
			return env, err
		}
		if rb != nil {
			env.Rollback = rb
			if rb.Eligible {
				env.Kind = Rollback
				env.Reason = fmt.Sprintf("%s: the release image returns to %s, an eligible rollback target (%s)", where, rb.Image, rb.Note)
			} else {
				env.Reason += "; not an eligible rollback: " + rb.Note
			}
		}
	case len(e.configChange) > 0 || e.renderCheck:
		env.Kind = Config
		if len(env.Changed) == 0 {
			env.Changed = []string{"render inputs"}
		}
		env.Reason = fmt.Sprintf("%s: configuration changed (%s); no new image reference", where, strings.Join(env.Changed, ", "))
		if e.renderCheck {
			env.Reason += "; only a render can show whether its images change"
		}
	default:
		env.Kind = NoOp
		env.Reason = where + ": no semantic change (comments, whitespace or key order only)"
	}
	return env, nil
}

func displayName(fc *fileChange) string {
	switch {
	case fc.headPath == "":
		return fc.basePath + " (removed)"
	case fc.basePath == "" && fc.gh.Status == "added":
		return fc.headPath + " (added)"
	case fc.basePath != "" && fc.basePath != fc.headPath:
		return fc.headPath + " (renamed from " + fc.basePath + ")"
	}
	return fc.headPath
}

// configChange lists what changed in one side of a file besides image references.
func configChange(sd side) []string {
	fc := sd.fc
	switch {
	case sd.inBase && !sd.inHead && fc.headPath != "":
		return []string{fc.basePath + ": moved to " + fc.headPath}
	case sd.inBase && !sd.inHead:
		return []string{fc.basePath + ": removed"}
	case !sd.inBase && sd.inHead && fc.basePath != "":
		return []string{fc.headPath + ": moved from " + fc.basePath}
	case sd.inBase && sd.inHead && fc.basePath != fc.headPath:
		return []string{fc.headPath + ": renamed from " + fc.basePath}
	case fc.ownChart:
		if fc.rawEqual {
			return nil
		}
		if fc.basePath == "" {
			return []string{fc.headPath + ": added (own chart file)"}
		}
		return []string{fc.headPath + ": own chart file"}
	}
	var baseDocs, headDocs []any
	if fc.base != nil {
		baseDocs = fc.base.stripped
	}
	if fc.head != nil {
		headDocs = fc.head.stripped
	}
	if fc.basePath == "" {
		if len(headDocs) == 0 {
			return nil // an added file with nothing in it
		}
		return []string{fc.headPath + ": added"}
	}
	var paths []string
	diffPaths(docsValue(baseDocs), docsValue(headDocs), "", &paths)
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, fc.headPath+": "+p)
	}
	return out
}

// docsValue: a single document compares as itself (paths without a document index); several as a list.
func docsValue(docs []any) any {
	if len(docs) == 1 {
		return docs[0]
	}
	if docs == nil {
		return []any{}
	}
	return docs
}

// renderChanged: something only a render can judge changed in this side of the file (spec section 5.2, step 8): an own chart's
// template side, or the render inputs (chart pins, a kustomization's remote bases) at base and head differ, in whatever part of
// the document (a new subtree, a whole new or removed file included).
func renderChanged(sd side) bool {
	if sd.fc.ownChart {
		return !(sd.inBase && sd.inHead && sd.fc.rawEqual && sd.fc.basePath == sd.fc.headPath)
	}
	name := sd.fc.headPath
	if name == "" {
		name = sd.fc.basePath
	}
	kust := kustomizationFile(sd.fc.headPath) || kustomizationFile(sd.fc.basePath)
	b, h := renderInputs(sd.baseDocs(), kust), renderInputs(sd.headDocs(), kust)
	if len(b) != len(h) {
		return true
	}
	for k, v := range b {
		if h[k] != v {
			return true
		}
	}
	return false
}

// renderKeys are keys whose values choose what is rendered without naming an image.
var renderKeys = map[string]bool{"targetRevision": true, "repoURL": true, "chart": true, "helmCharts": true}

// renderInputs is every leaf under a render key (path -> value), and for a kustomization every remote resources, bases or
// components entry.
func renderInputs(docs []any, kust bool) map[string]string {
	out := map[string]string{}
	var visit func(v any, p string, under bool)
	visit = func(v any, p string, under bool) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				visit(x, join(p, k), under || renderKeys[k])
			}
		case []any:
			for i, x := range t {
				visit(x, index(p, i), under)
			}
		default:
			if under {
				out[p] = fmt.Sprint(v)
			}
		}
	}
	for i, d := range docs {
		visit(d, fmt.Sprintf("#%d", i), false)
		if !kust {
			continue
		}
		for _, k := range []string{"resources", "bases", "components"} {
			l, _ := field(d, k).([]any)
			for n, x := range l {
				if s, ok := x.(string); ok && remote(s) {
					out[fmt.Sprintf("#%d.%s[%d]", i, k, n)] = s
				}
			}
		}
	}
	return out
}

func field(doc any, k string) any {
	if m, ok := doc.(map[string]any); ok {
		return m[k]
	}
	return nil
}

func remote(s string) bool {
	return strings.Contains(s, "://") || strings.HasPrefix(s, "github.com/") || strings.HasPrefix(s, "git@") || strings.Contains(s, "?ref=")
}

// rollback answers rollback eligibility when every new image reference of the environment is its release image (spec section
// 5.2, step 6); nil when it does not apply. The rule is the catalog's extract-promoted-image rollback-eligibility step: the release
// file is <cluster>/<env>/release.yaml, else values.yaml; the image is (release.image.source // release.image.repository //
// rollout.image.repository):(release.image.tag // rollout.image.tag) at head; it is eligible when one of the newest five release
// records of this app, environment and cluster that reached healthy (healthyAt set) ran that exact image. One addition: when the
// release image pins a digest, that record must have recorded the same digest (imageDigest), so a digest the history never ran is
// not a rollback. Records that cannot be read are an error (a doubt, not stored), not "not eligible": a failed read must not freeze.
func (j *job) rollback(ctx context.Context, e *envAcc, fresh []located) (*RollbackCheck, error) {
	if e.key.cluster == "*" || e.key.env == "*" {
		return nil, nil
	}
	release := ""
	var doc any
	for _, name := range []string{"release.yaml", "values.yaml"} {
		p := e.key.cluster + "/" + e.key.env + "/" + name
		for _, sd := range e.sides {
			if sd.inHead && sd.fc.headPath == p && sd.fc.head != nil && len(sd.fc.head.docs) > 0 {
				release, doc = p, sd.fc.head.docs[0]
				break
			}
		}
		if release != "" {
			break
		}
	}
	if release == "" {
		return nil, nil
	}
	for _, l := range fresh {
		if l.file != release || l.ref.Doc != 0 || (l.ref.Path != "release.image" && l.ref.Path != "rollout.image") {
			return nil, nil // another image reference is new: the release image alone decides nothing
		}
	}
	m := releaseFileRE.FindStringSubmatch(release)
	cluster, env := m[1], m[2]
	repo := firstScalar(doc, "release.image.source", "release.image.repository", "rollout.image.repository")
	tag := firstScalar(doc, "release.image.tag", "rollout.image.tag")
	digest := firstScalar(doc, "release.image.digest", "rollout.image.digest")
	if repo == "" || tag == "" {
		return &RollbackCheck{Note: fmt.Sprintf("no release.image.repository/tag (or the deprecated rollout.image) in %s", release)}, nil
	}
	image := repo + ":" + tag
	rb := &RollbackCheck{Image: image}
	if digest != "" {
		rb.Image = image + "@" + digest
	}
	recs, err := j.s.Records.ListEnv(ctx, j.app.Namespace, j.app.Name, env, cluster)
	if err != nil {
		return nil, &doubt{"could not read the release records for rollback eligibility: " + err.Error(), true}
	}
	type hr struct{ id, at, image, digest string }
	var healthy []hr
	for _, r := range recs {
		if at := r.Data["healthyAt"]; at != "" {
			healthy = append(healthy, hr{r.Data["releaseId"], at, r.Data["image"], r.Data["imageDigest"]})
		}
	}
	// jq: sort_by(.data.healthyAt) | reverse | .[0:5]
	sort.SliceStable(healthy, func(a, b int) bool { return healthy[a].at < healthy[b].at })
	for a, b := 0, len(healthy)-1; a < b; a, b = a+1, b-1 {
		healthy[a], healthy[b] = healthy[b], healthy[a]
	}
	if len(healthy) > 5 {
		healthy = healthy[:5]
	}
	for _, h := range healthy {
		if h.image == image && (digest == "" || h.digest == digest) {
			rb.Eligible = true
			rb.Note = fmt.Sprintf("%s ran healthy on %s/%s as release %s (healthy %s), one of its last five healthy releases", rb.Image, cluster, env, h.id, h.at)
			return rb, nil
		}
	}
	rb.Note = fmt.Sprintf("%s is not among the images of the last five healthy releases on %s/%s", rb.Image, cluster, env)
	return rb, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
