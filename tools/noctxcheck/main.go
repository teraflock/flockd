// Command noctxcheck runs the noctx analyzer (HTTP requests built without
// a context) as a standalone checker. Upstream only ships a unitchecker
// binary meant for `go vet -vettool`, which prints JSON to stdout, always
// exits 0, and is silenced by the vet cache on a rerun — none of which
// works as a CI gate. This wrapper is a singlechecker, so it exits 3 on
// findings and honours -test=false like bodyclose does.
//
// Its own module keeps the analyzer out of flockd's go.mod.
package main

import (
	"github.com/sonatard/noctx"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() { singlechecker.Main(noctx.Analyzer) }
