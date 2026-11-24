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
		Files []ghFile `json:"files"`
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
	if len(cmp.Files) >= compareFileLimit {
		return &doubt{fmt.Sprintf("the pull request changes %d or more files, and GitHub lists at most %d: it cannot show that nothing else changed", len(cmp.Files), compareFileLimit), false}
	}
	return j.classify(ctx, cmp.Files)
}

// inPull: head must be one of the pull request's commits (an earlier head a gate was started for).
func (j *job) inPull(ctx context.Context) error {
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
		if len(commits) < 100 {
			break
		}
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
	files        []*fileChange
	baseRefs     map[string]bool
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
		keys, err := j.envsOf(ctx, fc)
		if err != nil {
			return err
		}
		for _, k := range keys {
			e := envs[k]
			if e == nil {
				e = &envAcc{key: k, baseRefs: map[string]bool{}, headRefSet: map[string]bool{}, baseRefSet: map[string]bool{}}
				envs[k] = e
			}
			e.files = append(e.files, fc)
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

// envsOf maps a file to the environments it feeds: <cluster>/<env>/... is one; <cluster>/<file> feeds every environment directory
// under <cluster>/ (at base or head); a file at the root feeds the whole repository.
func (j *job) envsOf(ctx context.Context, fc *fileChange) ([]envKey, error) {
	seen := map[envKey]bool{}
	var out []envKey
	for _, p := range []string{fc.headPath, fc.basePath} {
		if p == "" {
			continue
		}
		segs := strings.Split(p, "/")
		var keys []envKey
		switch {
		case len(segs) >= 3:
			keys = []envKey{{segs[0], segs[1]}}
		case len(segs) == 2:
			names, err := j.envDirs(ctx, segs[0])
			if err != nil {
				return nil, err
			}
			for _, n := range names {
				keys = append(keys, envKey{segs[0], n})
			}
			if len(keys) == 0 {
				keys = []envKey{{segs[0], "*"}}
			}
		default:
			keys = []envKey{{"*", "*"}}
		}
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
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

// judge decides one environment's kind.
func (j *job) judge(ctx context.Context, e *envAcc) (Env, error) {
	env := Env{Cluster: e.key.cluster, Env: e.key.env, RenderCheck: RenderNone, Images: ImageSets{Base: []string{}, Head: []string{}}}
	for _, fc := range e.files {
		env.Files = append(env.Files, displayName(fc))
		if fc.base != nil {
			for _, r := range fc.base.refs {
				e.baseRefSet[r.Ref] = true
			}
		}
		if fc.head != nil {
			for _, r := range fc.head.refs {
				e.headRefs = append(e.headRefs, located{fc.headPath, r})
				e.headRefSet[r.Ref] = true
			}
		}
		what, render := configChange(fc)
		e.configChange = append(e.configChange, what...)
		e.renderCheck = e.renderCheck || render
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
		if rb := j.rollback(ctx, e, fresh); rb != nil {
			env.Rollback = rb
			if rb.Eligible {
				env.Kind = Rollback
				env.Reason = fmt.Sprintf("%s: the release image returns to %s, an eligible rollback target (%s)", where, rb.Image, rb.Note)
			} else {
				env.Reason += "; not an eligible rollback: " + rb.Note
			}
		}
	case len(e.configChange) > 0:
		env.Kind = Config
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

// configChange lists what changed in a file besides image references, and whether only a render can judge it.
func configChange(fc *fileChange) ([]string, bool) {
	name := fc.headPath
	if name == "" {
		name = fc.basePath
	}
	switch {
	case fc.headPath == "":
		return []string{name + ": removed"}, fc.ownChart
	case fc.basePath != "" && fc.basePath != fc.headPath:
		return []string{name + ": renamed from " + fc.basePath}, fc.ownChart
	case fc.ownChart:
		if fc.rawEqual {
			return nil, false
		}
		return []string{name + ": own chart file"}, true
	}
	var baseDocs, headDocs []any
	if fc.base != nil {
		baseDocs = fc.base.stripped
	}
	if fc.head != nil {
		headDocs = fc.head.stripped
	}
	if fc.basePath == "" && len(headDocs) == 0 {
		return nil, false // an added file with nothing in it
	}
	var paths []string
	diffPaths(docsValue(baseDocs), docsValue(headDocs), "", &paths)
	render := false
	for _, p := range paths {
		if renderPath(p) {
			render = true
		}
	}
	if kustomizationFile(name) && remoteBasesChanged(fc) {
		render = true
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, name+": "+p)
	}
	if fc.basePath == "" && len(out) > 0 {
		out = []string{name + ": added"}
	}
	return out, render
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

// renderPath: a configuration change that can change images with no image key moving (spec section 5.2, step 8).
func renderPath(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '.' || r == '[' || r == ']' }) {
		switch seg {
		case "targetRevision", "repoURL", "chart", "helmCharts":
			return true
		}
	}
	return false
}

// remoteBasesChanged: a kustomization's resources, bases or components changed and one of them (before or after) is remote.
func remoteBasesChanged(fc *fileChange) bool {
	for _, k := range []string{"resources", "bases", "components"} {
		var b, h any
		if fc.base != nil && len(fc.base.stripped) > 0 {
			b = field(fc.base.stripped[0], k)
		}
		if fc.head != nil && len(fc.head.stripped) > 0 {
			h = field(fc.head.stripped[0], k)
		}
		var changed []string
		diffPaths(b, h, k, &changed)
		if len(changed) > 0 && (hasRemote(b) || hasRemote(h)) {
			return true
		}
	}
	return false
}

func field(doc any, k string) any {
	if m, ok := doc.(map[string]any); ok {
		return m[k]
	}
	return nil
}

func hasRemote(v any) bool {
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		if strings.Contains(s, "://") || strings.HasPrefix(s, "github.com/") || strings.HasPrefix(s, "git@") || strings.Contains(s, "?ref=") {
			return true
		}
	}
	return false
}

// rollback answers rollback eligibility when every new image reference of the environment is its release image (spec section
// 5.2, step 6); nil when it does not apply. The rule is the catalog's extract-promoted-image rollback-eligibility step, unchanged:
// the release file is <cluster>/<env>/release.yaml, else values.yaml; the image is (release.image.source // release.image.repository
// // rollout.image.repository):(release.image.tag // rollout.image.tag) at head; it is eligible when one of the newest five release
// records of this app, environment and cluster that reached healthy (healthyAt set) ran that exact image. A failure to read the
// records is "not eligible".
func (j *job) rollback(ctx context.Context, e *envAcc, fresh []located) *RollbackCheck {
	if e.key.cluster == "*" || e.key.env == "*" {
		return nil
	}
	release := ""
	for _, name := range []string{"release.yaml", "values.yaml"} {
		p := e.key.cluster + "/" + e.key.env + "/" + name
		for _, fc := range e.files {
			if fc.headPath == p && fc.head != nil {
				release = p
				break
			}
		}
		if release != "" {
			break
		}
	}
	if release == "" {
		return nil
	}
	var doc any
	for _, l := range fresh {
		if l.file != release || l.ref.Doc != 0 || (l.ref.Path != "release.image" && l.ref.Path != "rollout.image") {
			return nil // another image reference is new: the release image alone decides nothing
		}
	}
	for _, fc := range e.files {
		if fc.headPath == release && len(fc.head.docs) > 0 {
			doc = fc.head.docs[0]
		}
	}
	m := releaseFileRE.FindStringSubmatch(release)
	cluster, env := m[1], m[2]
	repo := firstScalar(doc, "release.image.source", "release.image.repository", "rollout.image.repository")
	tag := firstScalar(doc, "release.image.tag", "rollout.image.tag")
	if repo == "" || tag == "" {
		return &RollbackCheck{Note: fmt.Sprintf("no release.image.repository/tag (or the deprecated rollout.image) in %s", release)}
	}
	image := repo + ":" + tag
	rb := &RollbackCheck{Image: image}
	recs, err := j.s.Records.ListEnv(ctx, j.app.Namespace, j.app.Name, env, cluster)
	if err != nil {
		rb.Note = "could not read the release records: " + err.Error()
		return rb
	}
	type hr struct{ id, at, image string }
	var healthy []hr
	for _, r := range recs {
		if at := r.Data["healthyAt"]; at != "" {
			healthy = append(healthy, hr{r.Data["releaseId"], at, r.Data["image"]})
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
		if h.image == image {
			rb.Eligible = true
			rb.Note = fmt.Sprintf("%s ran healthy on %s/%s as release %s (healthy %s), one of its last five healthy releases", image, cluster, env, h.id, h.at)
			return rb
		}
	}
	rb.Note = fmt.Sprintf("%s is not among the images of the last five healthy releases on %s/%s", image, cluster, env)
	return rb
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
