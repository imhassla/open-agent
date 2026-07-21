package tools

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// Anchored to the start of a line so `import`/`func main(` appearing inside a
// string literal or comment (mid-line) is not mistaken for a real declaration.
var (
	reImportBlock  = regexp.MustCompile(`(?ms)^[ \t]*import\s*\([^)]*\)`)
	reImportSingle = regexp.MustCompile(`(?m)^[ \t]*import\s+(?:[A-Za-z_.]\w*\s+)?"[^"]+"`)
	reFuncMain     = regexp.MustCompile(`(?m)^[ \t]*func\s+main\s*\(`)
)

// splitImports pulls import declarations out of a REPL snippet so they can be
// evaluated before the statements (yaegi rejects an import + a bare statement in
// one Eval — it switches to file mode where statements aren't declarations).
func splitImports(src string) ([]string, string) {
	var imports []string
	body := src
	for _, re := range []*regexp.Regexp{reImportBlock, reImportSingle} {
		imports = append(imports, re.FindAllString(body, -1)...)
		body = re.ReplaceAllString(body, "")
	}
	return imports, strings.Trim(body, " \t\r\n;")
}

// GoEval runs a Go snippet in-process via yaegi — no compilation, no temp files.
// REPL-style: include any imports it needs (e.g. `import "fmt"; fmt.Println(2+3)`).
// Returns captured output, or the last expression's value if there's no output.
// The whole stdlib is available; there is no OS-level sandbox (use --sandbox for
// untrusted code).
func GoEval(src string, timeoutSec int) (string, error) {
	if timeoutSec <= 0 {
		timeoutSec = 10
	}
	var out bytes.Buffer
	i := interp.New(interp.Options{Stdout: &out, Stderr: &out})
	if err := i.Use(stdlib.Symbols); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	imports, body := splitImports(src)
	for _, imp := range imports {
		if _, err := i.EvalWithContext(ctx, imp); err != nil {
			return "error: " + err.Error(), nil
		}
	}

	// yaegi's EvalWithContext does not reliably interrupt a tight/empty loop
	// (`for {}`), so run it on its own goroutine and hard-stop at the deadline —
	// otherwise one bad snippet wedges the whole agent turn. The stuck goroutine
	// is leaked (acceptable: the process is short-lived per tool call).
	type evalResult struct {
		v   reflect.Value
		err error
	}
	done := make(chan evalResult, 1)
	go func() {
		v, err := i.EvalWithContext(ctx, body)
		if err == nil && reFuncMain.MatchString(body) {
			_, err = i.EvalWithContext(ctx, "main()") // run an entrypoint if defined
		}
		done <- evalResult{v, err}
	}()

	var v reflect.Value
	var err error
	select {
	case <-ctx.Done():
		return fmt.Sprintf("error: evaluation timed out after %ds (possible infinite loop)", timeoutSec), nil
	case r := <-done:
		v, err = r.v, r.err
	}

	res := out.String()
	if err != nil {
		if res != "" {
			return res + "\nerror: " + err.Error(), nil
		}
		return "error: " + err.Error(), nil
	}
	if res == "" {
		if v.IsValid() && v.Kind() != 0 { // 0 == reflect.Invalid
			return fmt.Sprintf("%v", v), nil
		}
		return "(no output)", nil
	}
	return res, nil
}
