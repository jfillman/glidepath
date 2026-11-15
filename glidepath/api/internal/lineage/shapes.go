package lineage

import (
	"regexp"

	"github.com/glidepath/api/internal/pipelineruns"
)

// Shapes is the ONE table of value shapes for the lineage annotations. The resolver accepts only values that match it, and
// the API stamps only values that match it, so a value this API writes is never one admission refuses. The cluster's
// admission policy for glidepath-api runs (jfillman/apron, glidepath-api-pipelinerun-shape, the "promotion-lineage"
// validation) holds each key to the same pattern: keep the two identical (shapes_test.go carries the policy's literals).
var Shapes = map[string]string{
	pipelineruns.AnnotationOriginChainID:     `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
	pipelineruns.AnnotationOriginChainSlug:   `^[a-z]{2,20}-[a-z]{2,20}$`,
	pipelineruns.AnnotationOriginTraceID:     `^[0-9a-f]{32}$`,
	pipelineruns.AnnotationOriginTraceparent: `^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`,
	pipelineruns.AnnotationOriginFinishedAt:  `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.][0-9]{1,9})?Z$`,
}

var shapeRE = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for k, v := range Shapes {
		m[k] = regexp.MustCompile(v)
	}
	return m
}()

// Valid reports whether v is a value the annotation key may carry.
func Valid(key, v string) bool {
	re, ok := shapeRE[key]
	return ok && re.MatchString(v)
}

// Annotations is the lineage annotations of an origin, each checked against Shapes; ok is false (and nothing is returned) when
// any value is outside its shape. The slug is optional and left out when empty.
func (o *Origin) Annotations() (map[string]string, bool) {
	a := map[string]string{
		pipelineruns.AnnotationOriginChainID:     o.ChainID,
		pipelineruns.AnnotationOriginTraceID:     o.TraceID,
		pipelineruns.AnnotationOriginTraceparent: o.Traceparent,
		pipelineruns.AnnotationOriginFinishedAt:  o.FinishedAt,
	}
	if o.ChainSlug != "" {
		a[pipelineruns.AnnotationOriginChainSlug] = o.ChainSlug
	}
	for k, v := range a {
		if !Valid(k, v) {
			return nil, false
		}
	}
	return a, true
}
