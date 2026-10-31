package cicd

import (
	"errors"
	"io"
	"strings"
)

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

func errorsAs(err error, target any) bool { return errors.As(err, target) }
