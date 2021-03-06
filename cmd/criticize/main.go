package criticize

import (
	"encoding/json"
	"flag"
	"go/ast"
	"go/build"
	"go/parser"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/go-critic/go-critic/lint"
	"golang.org/x/tools/go/loader"
)

var generatedFileCommentRE = regexp.MustCompile("Code generated .* DO NOT EDIT.")

type linter struct {
	ctx *lint.Context

	prog *loader.Program

	checkers []*lint.Checker

	foundIssues bool // True if there any checker reported an issue

	// Command line flags:

	configFile string

	withOpinionated    bool
	withExperimental   bool
	checkGenerated     bool
	shorterErrLocation bool

	packages        []string
	enabledCheckers []string
	failureExitCode int
	checkerParams   map[string]map[string]interface{}
}

// Main implements gocritic sub-command entry point.
func Main() {
	var l linter
	parseArgv(&l)
	if l.configFile != "" {
		l.LoadConfig()
	}
	l.LoadProgram()
	l.InitCheckers()

	for _, pkgPath := range l.packages {
		l.CheckPackage(pkgPath)
	}

	os.Exit(l.ExitCode())
}

func blame(format string, args ...interface{}) {
	log.Printf(format, args...)
	flag.Usage()
	os.Exit(1)
}

// parseArgv processes command-line arguments and fills ctx argument with them.
// Terminates program on error.
func parseArgv(l *linter) {
	const enableAll = "all"

	flag.Usage = func() {
		log.Printf("usage: [flags] package...")
		flag.PrintDefaults()
	}

	enable := flag.String("enable", enableAll,
		`comma-separated list of enabled checkers`)
	flag.StringVar(&l.configFile, "config", "",
		`name of JSON file containing checkers configurations`)
	disable := flag.String("disable", "",
		`comma-separated list of disabled checkers`)
	flag.BoolVar(&l.withExperimental, `withExperimental`, false,
		`only for -enable=all, include experimental checks`)
	flag.BoolVar(&l.withOpinionated, `withOpinionated`, false,
		`only for -enable=all, include very opinionated checks`)
	flag.IntVar(&l.failureExitCode, "failcode", 1,
		`exit code to be used when lint issues are found`)
	flag.BoolVar(&l.checkGenerated, "checkGenerated", false,
		`whether to check machine-generated files`)
	flag.BoolVar(&l.shorterErrLocation, "shorterErrLocation", true,
		`whether to replace error location prefix with $GOROOT and $GOPATH`)

	flag.Parse()

	l.packages = flag.Args()

	if len(l.packages) == 0 {
		blame("no packages specified\n")
	}
	if *enable != enableAll && l.withExperimental {
		blame("-withExperimental used with -enable=%q", *enable)
	}
	if *enable != enableAll && l.withOpinionated {
		blame("-withOpinionated used with -enable=%q", *enable)
	}

	switch *enable {
	case enableAll:
		for _, rule := range lint.RuleList() {
			if rule.Experimental && !l.withExperimental {
				continue
			}
			if rule.VeryOpinionated && !l.withOpinionated {
				continue
			}
			l.enabledCheckers = append(l.enabledCheckers, rule.Name())
		}
	case "":
		// Empty slice. Semantically "disable-all".
		// Can be used to run all pipelines without actual checkers.
		l.enabledCheckers = []string{}
	default:
		// Comma-separated list of names.
		l.enabledCheckers = strings.Split(*enable, ",")
	}

	switch *disable {
	case "all":
		l.enabledCheckers = l.enabledCheckers[:0]
	case "":
		// nothing to disable, skip
	default:
		disabled := strings.Split(*disable, ",")
		filtred := l.enabledCheckers[:0]

		for _, e := range l.enabledCheckers {
			found := false
			for _, d := range disabled {
				if e == d {
					found = true
				}
			}
			if !found {
				filtred = append(filtred, e)
			}
		}
		l.enabledCheckers = filtred
	}
}

func (l *linter) LoadConfig() {
	raw, err := ioutil.ReadFile(l.configFile)
	if err != nil {
		log.Printf("cannot read config file %s, got error: %s", l.configFile, err)
		return
	}

	var params map[string]interface{}
	if err := json.Unmarshal(raw, &params); err != nil {
		log.Fatalf("cannot parse config file, got error: %s", err)
		return
	}

	l.checkerParams = make(map[string]map[string]interface{})
	for k, v := range params {
		if v, ok := v.(map[string]interface{}); ok {
			l.checkerParams[k] = v
		} else {
			log.Printf("cannot parse value for %v", k)
		}
	}
}

func (l *linter) LoadProgram() {
	sizes := types.SizesFor("gc", runtime.GOARCH)
	if sizes == nil {
		log.Fatalf("can't find sizes info for %s", runtime.GOARCH)
	}

	conf := loader.Config{
		ParserMode: parser.ParseComments,
		TypeChecker: types.Config{
			Sizes: sizes,
		},
	}

	if _, err := conf.FromArgs(l.packages, true); err != nil {
		log.Fatalf("resolve packages: %v", err)
	}
	prog, err := conf.Load()
	if err != nil {
		log.Fatalf("load program: %v", err)
	}

	l.prog = prog
	l.ctx = lint.NewContext(prog.Fset, sizes)
}

func (l *linter) InitCheckers() {
	requested := make(map[string]bool)
	available := lint.RuleList()

	if l.enabledCheckers == nil {
		// Fill default checkers set.
		for _, rule := range available {
			if rule.Experimental && !l.withExperimental {
				continue
			}
			if rule.VeryOpinionated && !l.withOpinionated {
				continue
			}
			requested[rule.Name()] = true
		}
	} else {
		for _, name := range l.enabledCheckers {
			requested[name] = true
		}
	}

	for _, rule := range available {
		if !requested[rule.Name()] {
			continue
		}
		l.checkers = append(l.checkers, lint.NewChecker(
			rule,
			l.ctx,
			l.checkerParams[rule.Name()],
		))
		delete(requested, rule.Name())
	}

	if len(requested) != 0 {
		for name := range requested {
			log.Printf("%s: checker not found", name)
		}
		log.Fatalf("exiting due to initialization failure")
	}
}

func (l *linter) CheckPackage(pkgPath string) {
	pkgInfo := l.prog.Imported[pkgPath]
	if pkgInfo == nil || !pkgInfo.TransitivelyErrorFree {
		log.Fatalf("%s package is not properly loaded", pkgPath)
	}

	l.ctx.SetPackageInfo(&pkgInfo.Info, pkgInfo.Pkg)
	for _, f := range pkgInfo.Files {
		if l.checkGenerated || !isGenerated(f) {
			l.ctx.SetFileInfo(l.getFilename(f))
			l.checkFile(f)
		}
	}
}

func isGenerated(f *ast.File) bool {
	return len(f.Comments) != 0 && generatedFileCommentRE.MatchString(f.Comments[0].Text())
}

func (l *linter) getFilename(f *ast.File) string {
	// see https://github.com/golang/go/issues/24498
	return filepath.Base(l.prog.Fset.Position(f.Pos()).Filename)
}

// ExitCode returns status code that should be used as an argument to os.Exit.
func (l *linter) ExitCode() int {
	if l.foundIssues {
		return l.failureExitCode
	}
	return 0
}

func (l *linter) checkFile(f *ast.File) {
	var wg sync.WaitGroup
	wg.Add(len(l.checkers))
	for _, c := range l.checkers {
		// All checkers are expected to use *lint.Context
		// as read-only structure, so no copying is required.
		go func(c *lint.Checker) {
			defer func() {
				wg.Done()
				// Checker signals unexpected error with panic(error).
				r := recover()
				if r == nil {
					return // There were no panic
				}
				if err, ok := r.(error); ok {
					log.Printf("%s: error: %v\n", c.Rule, err)
					panic(err)
				} else {
					// Some other kind of run-time panic.
					// Undo the recover and resume panic.
					panic(r)
				}
			}()

			for _, warn := range c.Check(f) {
				l.foundIssues = true
				loc := l.ctx.FileSet().Position(warn.Node.Pos()).String()
				if l.shorterErrLocation {
					loc = shortenLocation(loc)
				}
				log.Printf("%s: %s: %v\n", loc, c.Rule, warn.Text)
			}
		}(c)
	}
	wg.Wait()
}

func shortenLocation(loc string) string {
	switch {
	case strings.HasPrefix(loc, build.Default.GOPATH):
		return strings.Replace(loc, build.Default.GOPATH, "$GOPATH", 1)
	case strings.HasPrefix(loc, build.Default.GOROOT):
		return strings.Replace(loc, build.Default.GOROOT, "$GOROOT", 1)
	default:
		return loc
	}
}
