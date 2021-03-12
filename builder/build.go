// Package builder is the compiler driver of TinyGo. It takes in a package name
// and an output path, and outputs an executable. It manages the entire
// compilation pipeline in between.
package builder

import (
	"crypto/sha512"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/types"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/tinygo-org/tinygo/compileopts"
	"github.com/tinygo-org/tinygo/compiler"
	"github.com/tinygo-org/tinygo/goenv"
	"github.com/tinygo-org/tinygo/interp"
	"github.com/tinygo-org/tinygo/loader"
	"github.com/tinygo-org/tinygo/stacksize"
	"github.com/tinygo-org/tinygo/transform"
	"tinygo.org/x/go-llvm"
)

// BuildResult is the output of a build. This includes the binary itself and
// some other metadata that is obtained while building the binary.
type BuildResult struct {
	// A path to the output binary. It will be removed after Build returns, so
	// if it should be kept it must be copied or moved away.
	Binary string

	// The directory of the main package. This is useful for testing as the test
	// binary must be run in the directory of the tested package.
	MainDir string
}

// packageAction is the struct that is serialized to JSON and hashed, to work as
// a cache key of compiled packages. It should contain all the information that
// goes into a compiled package to avoid using stale data.
//
// Right now it's still important to include a hash of every import, because a
// dependency might have a public constant that this package uses and thus this
// package will need to be recompiled if that constant changes. In the future,
// the type data should be serialized to disk which can then be used as cache
// key, avoiding the need for recompiling all dependencies when only the
// implementation of an imported package changes.
type packageAction struct {
	ImportPath      string
	CompilerVersion int // compiler.Version
	LLVMVersion     string
	Config          *compiler.Config
	CFlags          []string
	FileHashes      map[string]string // hash of every file that's part of the package
	Imports         map[string]string // map from imported package to action ID hash
}

// Build performs a single package to executable Go build. It takes in a package
// name, an output path, and set of compile options and from that it manages the
// whole compilation process.
//
// The error value may be of type *MultiError. Callers will likely want to check
// for this case and print such errors individually.
func Build(pkgName, outpath string, config *compileopts.Config, action func(BuildResult) error) error {
	// Create a temporary directory for intermediary files.
	dir, err := ioutil.TempDir("", "tinygo")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	compilerConfig := &compiler.Config{
		Triple:          config.Triple(),
		CPU:             config.CPU(),
		Features:        config.Features(),
		GOOS:            config.GOOS(),
		GOARCH:          config.GOARCH(),
		CodeModel:       config.CodeModel(),
		RelocationModel: config.RelocationModel(),

		Scheduler:          config.Scheduler(),
		FuncImplementation: config.FuncImplementation(),
		AutomaticStackSize: config.AutomaticStackSize(),
		DefaultStackSize:   config.Target.DefaultStackSize,
		NeedsStackObjects:  config.NeedsStackObjects(),
		Debug:              config.Debug(),
	}

	// Load the target machine, which is the LLVM object that contains all
	// details of a target (alignment restrictions, pointer size, default
	// address spaces, etc).
	machine, err := compiler.NewTargetMachine(compilerConfig)
	if err != nil {
		return err
	}

	// Load entire program AST into memory.
	lprogram, err := loader.Load(config, []string{pkgName}, config.ClangHeaders, types.Config{
		Sizes: compiler.Sizes(machine),
	})
	if err != nil {
		return err
	}
	err = lprogram.Parse()
	if err != nil {
		return err
	}

	// The slice of jobs that orchestrates most of the build.
	// This is somewhat like an in-memory Makefile with each job being a
	// Makefile target.
	var jobs []*compileJob

	// Create the *ssa.Program. This does not yet build the entire SSA of the
	// program so it's pretty fast and doesn't need to be parallelized.
	program := lprogram.LoadSSA()

	// Add jobs to compile each package.
	// Packages that have a cache hit will not be compiled again.
	var packageJobs []*compileJob
	packageBitcodePaths := make(map[string]string)
	packageActionIDs := make(map[string]string)
	for _, pkg := range lprogram.Sorted() {
		pkg := pkg // necessary to avoid a race condition

		// Create a cache key: a hash from the action ID below that contains all
		// the parameters for the build.
		actionID := packageAction{
			ImportPath:      pkg.ImportPath,
			CompilerVersion: compiler.Version,
			LLVMVersion:     llvm.Version,
			Config:          compilerConfig,
			CFlags:          pkg.CFlags,
			FileHashes:      make(map[string]string, len(pkg.FileHashes)),
			Imports:         make(map[string]string, len(pkg.Pkg.Imports())),
		}
		for filePath, hash := range pkg.FileHashes {
			actionID.FileHashes[filePath] = hex.EncodeToString(hash)
		}
		for _, imported := range pkg.Pkg.Imports() {
			hash, ok := packageActionIDs[imported.Path()]
			if !ok {
				return fmt.Errorf("package %s imports %s but couldn't find dependency", pkg.ImportPath, imported.Path())
			}
			actionID.Imports[imported.Path()] = hash
		}
		buf, err := json.Marshal(actionID)
		if err != nil {
			panic(err) // shouldn't happen
		}
		hash := sha512.Sum512_224(buf)
		packageActionIDs[pkg.ImportPath] = hex.EncodeToString(hash[:])

		// Determine the path of the bitcode file (which is a serialized version
		// of a LLVM module).
		cacheDir := goenv.Get("GOCACHE")
		if cacheDir == "off" {
			// Use temporary build directory instead, effectively disabling the
			// build cache.
			cacheDir = dir
		}
		bitcodePath := filepath.Join(cacheDir, "pkg-"+hex.EncodeToString(hash[:])+".bc")
		packageBitcodePaths[pkg.ImportPath] = bitcodePath

		// Check whether this package has been compiled before, and if so don't
		// compile it again.
		if _, err := os.Stat(bitcodePath); err == nil {
			// Already cached, don't recreate this package.
			continue
		}

		// The package has not yet been compiled, so create a job to do so.
		job := &compileJob{
			description: "compile package " + pkg.ImportPath,
			run: func(*compileJob) error {
				// Compile AST to IR. The compiler.CompilePackage function will
				// build the SSA as needed.
				mod, errs := compiler.CompilePackage(pkg.ImportPath, pkg, program.Package(pkg.Pkg), machine, compilerConfig, config.DumpSSA())
				if errs != nil {
					return newMultiError(errs)
				}
				if err := llvm.VerifyModule(mod, llvm.PrintMessageAction); err != nil {
					return errors.New("verification error after compiling package " + pkg.ImportPath)
				}

				// Serialize the LLVM module as a bitcode file.
				// Write to a temporary path that is renamed to the destination
				// file to avoid race conditions with other TinyGo invocatiosn
				// that might also be compiling this package at the same time.
				f, err := ioutil.TempFile(filepath.Dir(bitcodePath), filepath.Base(bitcodePath))
				if err != nil {
					return err
				}
				if runtime.GOOS == "windows" {
					// Work around a problem on Windows.
					// For some reason, WriteBitcodeToFile causes TinyGo to
					// exit with the following message:
					//   LLVM ERROR: IO failure on output stream: Bad file descriptor
					buf := llvm.WriteBitcodeToMemoryBuffer(mod)
					defer buf.Dispose()
					_, err = f.Write(buf.Bytes())
				} else {
					// Otherwise, write bitcode directly to the file (probably
					// faster).
					err = llvm.WriteBitcodeToFile(mod, f)
				}
				if err != nil {
					// WriteBitcodeToFile doesn't produce a useful error on its
					// own, so create a somewhat useful error message here.
					return fmt.Errorf("failed to write bitcode for package %s to file %s", pkg.ImportPath, bitcodePath)
				}
				err = f.Close()
				if err != nil {
					return err
				}
				return os.Rename(f.Name(), bitcodePath)
			},
		}
		jobs = append(jobs, job)
		packageJobs = append(packageJobs, job)
	}

	// Add job that links and optimizes all packages together.
	var mod llvm.Module
	var stackSizeLoads []string
	programJob := &compileJob{
		description:  "link+optimize packages (LTO)",
		dependencies: packageJobs,
		run: func(*compileJob) error {
			// Load and link all the bitcode files. This does not yet optimize
			// anything, it only links the bitcode files together.
			ctx := llvm.NewContext()
			mod = ctx.NewModule("")
			for _, pkg := range lprogram.Sorted() {
				pkgMod, err := ctx.ParseBitcodeFile(packageBitcodePaths[pkg.ImportPath])
				if err != nil {
					return fmt.Errorf("failed to load bitcode file: %w", err)
				}
				err = llvm.LinkModules(mod, pkgMod)
				if err != nil {
					return fmt.Errorf("failed to link module: %w", err)
				}
			}

			// Create runtime.initAll function that calls the runtime
			// initializer of each package.
			llvmInitFn := mod.NamedFunction("runtime.initAll")
			llvmInitFn.SetLinkage(llvm.InternalLinkage)
			llvmInitFn.SetUnnamedAddr(true)
			llvmInitFn.Param(0).SetName("context")
			llvmInitFn.Param(1).SetName("parentHandle")
			block := mod.Context().AddBasicBlock(llvmInitFn, "entry")
			irbuilder := mod.Context().NewBuilder()
			defer irbuilder.Dispose()
			irbuilder.SetInsertPointAtEnd(block)
			i8ptrType := llvm.PointerType(mod.Context().Int8Type(), 0)
			for _, pkg := range lprogram.Sorted() {
				pkgInit := mod.NamedFunction(pkg.Pkg.Path() + ".init")
				if pkgInit.IsNil() {
					panic("init not found for " + pkg.Pkg.Path())
				}
				irbuilder.CreateCall(pkgInit, []llvm.Value{llvm.Undef(i8ptrType), llvm.Undef(i8ptrType)}, "")
			}
			irbuilder.CreateRetVoid()

			// After linking, functions should (as far as possible) be set to
			// private linkage or internal linkage. The compiler package marks
			// non-exported functions by setting the visibility to hidden or
			// (for thunks) to linkonce_odr linkage. Change the linkage here to
			// internal to benefit much more from interprocedural optimizations.
			for fn := mod.FirstFunction(); !fn.IsNil(); fn = llvm.NextFunction(fn) {
				if fn.Visibility() == llvm.HiddenVisibility {
					fn.SetVisibility(llvm.DefaultVisibility)
					fn.SetLinkage(llvm.InternalLinkage)
				} else if fn.Linkage() == llvm.LinkOnceODRLinkage {
					fn.SetLinkage(llvm.InternalLinkage)
				}
			}

			// Do the same for globals.
			for global := mod.FirstGlobal(); !global.IsNil(); global = llvm.NextGlobal(global) {
				if global.Visibility() == llvm.HiddenVisibility {
					global.SetVisibility(llvm.DefaultVisibility)
					global.SetLinkage(llvm.InternalLinkage)
				} else if global.Linkage() == llvm.LinkOnceODRLinkage {
					global.SetLinkage(llvm.InternalLinkage)
				}
			}

			if config.Options.PrintIR {
				fmt.Println("; Generated LLVM IR:")
				fmt.Println(mod.String())
			}

			// Run all optimization passes, which are much more effective now
			// that the optimizer can see the whole program at once.
			err := optimizeProgram(mod, config)
			if err != nil {
				return err
			}

			// Make sure stack sizes are loaded from a separate section so they can be
			// modified after linking.
			if config.AutomaticStackSize() {
				stackSizeLoads = transform.CreateStackSizeLoads(mod, config)
			}
			return nil
		},
	}
	jobs = append(jobs, programJob)

	// Check whether we only need to create an object file.
	// If so, we don't need to link anything and will be finished quickly.
	outext := filepath.Ext(outpath)
	if outext == ".o" || outext == ".bc" || outext == ".ll" {
		// Run jobs to produce the LLVM module.
		err := runJobs(jobs)
		if err != nil {
			return err
		}
		// Generate output.
		switch outext {
		case ".o":
			llvmBuf, err := machine.EmitToMemoryBuffer(mod, llvm.ObjectFile)
			if err != nil {
				return err
			}
			return ioutil.WriteFile(outpath, llvmBuf.Bytes(), 0666)
		case ".bc":
			data := llvm.WriteBitcodeToMemoryBuffer(mod).Bytes()
			return ioutil.WriteFile(outpath, data, 0666)
		case ".ll":
			data := []byte(mod.String())
			return ioutil.WriteFile(outpath, data, 0666)
		default:
			panic("unreachable")
		}
	}

	// Act as a compiler driver, as we need to produce a complete executable.
	// First add all jobs necessary to build this object file, then afterwards
	// run all jobs in parallel as far as possible.

	// Add job to write the output object file.
	objfile := filepath.Join(dir, "main.o")
	outputObjectFileJob := &compileJob{
		description:  "generate output file",
		dependencies: []*compileJob{programJob},
		result:       objfile,
		run: func(*compileJob) error {
			llvmBuf, err := machine.EmitToMemoryBuffer(mod, llvm.ObjectFile)
			if err != nil {
				return err
			}
			return ioutil.WriteFile(objfile, llvmBuf.Bytes(), 0666)
		},
	}
	jobs = append(jobs, outputObjectFileJob)

	// Prepare link command.
	linkerDependencies := []*compileJob{outputObjectFileJob}
	executable := filepath.Join(dir, "main")
	tmppath := executable // final file
	ldflags := append(config.LDFlags(), "-o", executable)

	// Add compiler-rt dependency if needed. Usually this is a simple load from
	// a cache.
	if config.Target.RTLib == "compiler-rt" {
		job, err := CompilerRT.load(config.Triple(), config.CPU(), dir)
		if err != nil {
			return err
		}
		jobs = append(jobs, job.dependencies...)
		jobs = append(jobs, job)
		linkerDependencies = append(linkerDependencies, job)
	}

	// Add libc dependency if needed.
	root := goenv.Get("TINYGOROOT")
	switch config.Target.Libc {
	case "picolibc":
		job, err := Picolibc.load(config.Triple(), config.CPU(), dir)
		if err != nil {
			return err
		}
		// The library needs to be compiled (cache miss).
		jobs = append(jobs, job.dependencies...)
		jobs = append(jobs, job)
		linkerDependencies = append(linkerDependencies, job)
	case "wasi-libc":
		path := filepath.Join(root, "lib/wasi-libc/sysroot/lib/wasm32-wasi/libc.a")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return errors.New("could not find wasi-libc, perhaps you need to run `make wasi-libc`?")
		}
		ldflags = append(ldflags, path)
	case "":
		// no library specified, so nothing to do
	default:
		return fmt.Errorf("unknown libc: %s", config.Target.Libc)
	}

	// Add jobs to compile extra files. These files are in C or assembly and
	// contain things like the interrupt vector table and low level operations
	// such as stack switching.
	for _, path := range config.ExtraFiles() {
		abspath := filepath.Join(root, path)
		job := &compileJob{
			description: "compile extra file " + path,
			run: func(job *compileJob) error {
				result, err := compileAndCacheCFile(abspath, dir, config)
				job.result = result
				return err
			},
		}
		jobs = append(jobs, job)
		linkerDependencies = append(linkerDependencies, job)
	}

	// Add jobs to compile C files in all packages. This is part of CGo.
	// TODO: do this as part of building the package to be able to link the
	// bitcode files together.
	for _, pkg := range lprogram.Sorted() {
		for _, filename := range pkg.CFiles {
			abspath := filepath.Join(pkg.Dir, filename)
			job := &compileJob{
				description: "compile CGo file " + abspath,
				run: func(job *compileJob) error {
					result, err := compileAndCacheCFile(abspath, dir, config)
					job.result = result
					return err
				},
			}
			jobs = append(jobs, job)
			linkerDependencies = append(linkerDependencies, job)
		}
	}

	// Linker flags from CGo lines:
	//     #cgo LDFLAGS: foo
	if len(lprogram.LDFlags) > 0 {
		ldflags = append(ldflags, lprogram.LDFlags...)
	}

	// Create a linker job, which links all object files together and does some
	// extra stuff that can only be done after linking.
	jobs = append(jobs, &compileJob{
		description:  "link",
		dependencies: linkerDependencies,
		run: func(job *compileJob) error {
			for _, dependency := range job.dependencies {
				if dependency.result == "" {
					return errors.New("dependency without result: " + dependency.description)
				}
				ldflags = append(ldflags, dependency.result)
			}
			if config.Options.PrintCommands {
				fmt.Printf("%s %s\n", config.Target.Linker, strings.Join(ldflags, " "))
			}
			err = link(config.Target.Linker, ldflags...)
			if err != nil {
				return &commandError{"failed to link", executable, err}
			}

			var calculatedStacks []string
			var stackSizes map[string]functionStackSize
			if config.Options.PrintStacks || config.AutomaticStackSize() {
				// Try to determine stack sizes at compile time.
				// Don't do this by default as it usually doesn't work on
				// unsupported architectures.
				calculatedStacks, stackSizes, err = determineStackSizes(mod, executable)
				if err != nil {
					return err
				}
			}
			if config.AutomaticStackSize() {
				// Modify the .tinygo_stacksizes section that contains a stack size
				// for each goroutine.
				err = modifyStackSizes(executable, stackSizeLoads, stackSizes)
				if err != nil {
					return fmt.Errorf("could not modify stack sizes: %w", err)
				}
			}

			if config.Options.PrintSizes == "short" || config.Options.PrintSizes == "full" {
				sizes, err := loadProgramSize(executable)
				if err != nil {
					return err
				}
				if config.Options.PrintSizes == "short" {
					fmt.Printf("   code    data     bss |   flash     ram\n")
					fmt.Printf("%7d %7d %7d | %7d %7d\n", sizes.Code, sizes.Data, sizes.BSS, sizes.Code+sizes.Data, sizes.Data+sizes.BSS)
				} else {
					fmt.Printf("   code  rodata    data     bss |   flash     ram | package\n")
					for _, name := range sizes.sortedPackageNames() {
						pkgSize := sizes.Packages[name]
						fmt.Printf("%7d %7d %7d %7d | %7d %7d | %s\n", pkgSize.Code, pkgSize.ROData, pkgSize.Data, pkgSize.BSS, pkgSize.Flash(), pkgSize.RAM(), name)
					}
					fmt.Printf("%7d %7d %7d %7d | %7d %7d | (sum)\n", sizes.Sum.Code, sizes.Sum.ROData, sizes.Sum.Data, sizes.Sum.BSS, sizes.Sum.Flash(), sizes.Sum.RAM())
					fmt.Printf("%7d       - %7d %7d | %7d %7d | (all)\n", sizes.Code, sizes.Data, sizes.BSS, sizes.Code+sizes.Data, sizes.Data+sizes.BSS)
				}
			}

			// Print goroutine stack sizes, as far as possible.
			if config.Options.PrintStacks {
				printStacks(calculatedStacks, stackSizes)
			}

			return nil
		},
	})

	// Run all jobs to compile and link the program.
	// Do this now (instead of after elf-to-hex and similar conversions) as it
	// is simpler and cannot be parallelized.
	err = runJobs(jobs)
	if err != nil {
		return err
	}

	// Get an Intel .hex file or .bin file from the .elf file.
	outputBinaryFormat := config.BinaryFormat(outext)
	switch outputBinaryFormat {
	case "elf":
		// do nothing, file is already in ELF format
	case "hex", "bin":
		// Extract raw binary, either encoding it as a hex file or as a raw
		// firmware file.
		tmppath = filepath.Join(dir, "main"+outext)
		err := objcopy(executable, tmppath, outputBinaryFormat)
		if err != nil {
			return err
		}
	case "uf2":
		// Get UF2 from the .elf file.
		tmppath = filepath.Join(dir, "main"+outext)
		err := convertELFFileToUF2File(executable, tmppath, config.Target.UF2FamilyID)
		if err != nil {
			return err
		}
	case "esp32", "esp8266":
		// Special format for the ESP family of chips (parsed by the ROM
		// bootloader).
		tmppath = filepath.Join(dir, "main"+outext)
		err := makeESPFirmareImage(executable, tmppath, outputBinaryFormat)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown output binary format: %s", outputBinaryFormat)
	}
	return action(BuildResult{
		Binary:  tmppath,
		MainDir: lprogram.MainPkg().Dir,
	})
}

// optimizeProgram runs a series of optimizations and transformations that are
// needed to convert a program to its final form. Some transformations are not
// optional and must be run as the compiler expects them to run.
func optimizeProgram(mod llvm.Module, config *compileopts.Config) error {
	err := interp.Run(mod, config.DumpSSA())
	if err != nil {
		return err
	}
	if err := llvm.VerifyModule(mod, llvm.PrintMessageAction); err != nil {
		return errors.New("verification error after interpreting runtime.initAll")
	}

	if config.GOOS() != "darwin" {
		transform.ApplyFunctionSections(mod) // -ffunction-sections
	}

	// Browsers cannot handle external functions that have type i64 because it
	// cannot be represented exactly in JavaScript (JS only has doubles). To
	// keep functions interoperable, pass int64 types as pointers to
	// stack-allocated values.
	// Use -wasm-abi=generic to disable this behaviour.
	if config.WasmAbi() == "js" {
		err := transform.ExternalInt64AsPtr(mod)
		if err != nil {
			return err
		}
	}

	// Optimization levels here are roughly the same as Clang, but probably not
	// exactly.
	var errs []error
	switch config.Options.Opt {
	/*
		Currently, turning optimizations off causes compile failures.
		We rely on the optimizer removing some dead symbols.
		Avoid providing an option that does not work right now.
		In the future once everything has been fixed we can re-enable this.

		case "none", "0":
			errs = transform.Optimize(mod, config, 0, 0, 0) // -O0
	*/
	case "1":
		errs = transform.Optimize(mod, config, 1, 0, 0) // -O1
	case "2":
		errs = transform.Optimize(mod, config, 2, 0, 225) // -O2
	case "s":
		errs = transform.Optimize(mod, config, 2, 1, 225) // -Os
	case "z":
		errs = transform.Optimize(mod, config, 2, 2, 5) // -Oz, default
	default:
		return errors.New("unknown optimization level: -opt=" + config.Options.Opt)
	}
	if len(errs) > 0 {
		return newMultiError(errs)
	}
	if err := llvm.VerifyModule(mod, llvm.PrintMessageAction); err != nil {
		return errors.New("verification failure after LLVM optimization passes")
	}

	// LLVM 11 by default tries to emit tail calls (even with the target feature
	// disabled) unless it is explicitly disabled with a function attribute.
	// This is a problem, as it tries to emit them and prints an error when it
	// can't with this feature disabled.
	// Because as of september 2020 tail calls are not yet widely supported,
	// they need to be disabled until they are widely supported (at which point
	// the +tail-call target feautre can be set).
	if strings.HasPrefix(config.Triple(), "wasm") {
		transform.DisableTailCalls(mod)
	}

	return nil
}

// functionStackSizes keeps stack size information about a single function
// (usually a goroutine).
type functionStackSize struct {
	humanName        string
	stackSize        uint64
	stackSizeType    stacksize.SizeType
	missingStackSize *stacksize.CallNode
}

// determineStackSizes tries to determine the stack sizes of all started
// goroutines and of the reset vector. The LLVM module is necessary to find
// functions that call a function pointer.
func determineStackSizes(mod llvm.Module, executable string) ([]string, map[string]functionStackSize, error) {
	var callsIndirectFunction []string
	gowrappers := []string{}
	gowrapperNames := make(map[string]string)
	for fn := mod.FirstFunction(); !fn.IsNil(); fn = llvm.NextFunction(fn) {
		// Determine which functions call a function pointer.
		for bb := fn.FirstBasicBlock(); !bb.IsNil(); bb = llvm.NextBasicBlock(bb) {
			for inst := bb.FirstInstruction(); !inst.IsNil(); inst = llvm.NextInstruction(inst) {
				if inst.IsACallInst().IsNil() {
					continue
				}
				if callee := inst.CalledValue(); callee.IsAFunction().IsNil() && callee.IsAInlineAsm().IsNil() {
					callsIndirectFunction = append(callsIndirectFunction, fn.Name())
				}
			}
		}

		// Get a list of "go wrappers", small wrapper functions that decode
		// parameters when starting a new goroutine.
		attr := fn.GetStringAttributeAtIndex(-1, "tinygo-gowrapper")
		if !attr.IsNil() {
			gowrappers = append(gowrappers, fn.Name())
			gowrapperNames[fn.Name()] = attr.GetStringValue()
		}
	}
	sort.Strings(gowrappers)

	// Load the ELF binary.
	f, err := elf.Open(executable)
	if err != nil {
		return nil, nil, fmt.Errorf("could not load executable for stack size analysis: %w", err)
	}
	defer f.Close()

	// Determine the frame size of each function (if available) and the callgraph.
	functions, err := stacksize.CallGraph(f, callsIndirectFunction)
	if err != nil {
		return nil, nil, fmt.Errorf("could not parse executable for stack size analysis: %w", err)
	}

	// Goroutines need to be started and finished and take up some stack space
	// that way. This can be measured by measuing the stack size of
	// tinygo_startTask.
	if numFuncs := len(functions["tinygo_startTask"]); numFuncs != 1 {
		return nil, nil, fmt.Errorf("expected exactly one definition of tinygo_startTask, got %d", numFuncs)
	}
	baseStackSize, baseStackSizeType, baseStackSizeFailedAt := functions["tinygo_startTask"][0].StackSize()

	sizes := make(map[string]functionStackSize)

	// Add the reset handler function, for convenience. The reset handler runs
	// startup code and the scheduler. The listed stack size is not the full
	// stack size: interrupts are not counted.
	var resetFunction string
	switch f.Machine {
	case elf.EM_ARM:
		// Note: all interrupts happen on this stack so the real size is bigger.
		resetFunction = "Reset_Handler"
	}
	if resetFunction != "" {
		funcs := functions[resetFunction]
		if len(funcs) != 1 {
			return nil, nil, fmt.Errorf("expected exactly one definition of %s in the callgraph, found %d", resetFunction, len(funcs))
		}
		stackSize, stackSizeType, missingStackSize := funcs[0].StackSize()
		sizes[resetFunction] = functionStackSize{
			stackSize:        stackSize,
			stackSizeType:    stackSizeType,
			missingStackSize: missingStackSize,
			humanName:        resetFunction,
		}
	}

	// Add all goroutine wrapper functions.
	for _, name := range gowrappers {
		funcs := functions[name]
		if len(funcs) != 1 {
			return nil, nil, fmt.Errorf("expected exactly one definition of %s in the callgraph, found %d", name, len(funcs))
		}
		humanName := gowrapperNames[name]
		if humanName == "" {
			humanName = name // fallback
		}
		stackSize, stackSizeType, missingStackSize := funcs[0].StackSize()
		if baseStackSizeType != stacksize.Bounded {
			// It was not possible to determine the stack size at compile time
			// because tinygo_startTask does not have a fixed stack size. This
			// can happen when using -opt=1.
			stackSizeType = baseStackSizeType
			missingStackSize = baseStackSizeFailedAt
		} else if stackSize < baseStackSize {
			// This goroutine has a very small stack, but still needs to fit all
			// registers to start and suspend the goroutine. Otherwise a stack
			// overflow will occur even before the goroutine is started.
			stackSize = baseStackSize
		}
		sizes[name] = functionStackSize{
			stackSize:        stackSize,
			stackSizeType:    stackSizeType,
			missingStackSize: missingStackSize,
			humanName:        humanName,
		}
	}

	if resetFunction != "" {
		return append([]string{resetFunction}, gowrappers...), sizes, nil
	}
	return gowrappers, sizes, nil
}

// modifyStackSizes modifies the .tinygo_stacksizes section with the updated
// stack size information. Before this modification, all stack sizes in the
// section assume the default stack size (which is relatively big).
func modifyStackSizes(executable string, stackSizeLoads []string, stackSizes map[string]functionStackSize) error {
	fp, err := os.OpenFile(executable, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer fp.Close()

	elfFile, err := elf.NewFile(fp)
	if err != nil {
		return err
	}

	section := elfFile.Section(".tinygo_stacksizes")
	if section == nil {
		return errors.New("could not find .tinygo_stacksizes section")
	}

	if section.Size != section.FileSize {
		// Sanity check.
		return fmt.Errorf("expected .tinygo_stacksizes to have identical size and file size, got %d and %d", section.Size, section.FileSize)
	}

	// Read all goroutine stack sizes.
	data := make([]byte, section.Size)
	_, err = fp.ReadAt(data, int64(section.Offset))
	if err != nil {
		return err
	}

	if len(stackSizeLoads)*4 != len(data) {
		// Note: while AVR should use 2 byte stack sizes, even 64-bit platforms
		// should probably stick to 4 byte stack sizes as a larger than 4GB
		// stack doesn't make much sense.
		return errors.New("expected 4 byte stack sizes")
	}

	// Modify goroutine stack sizes with a compile-time known worst case stack
	// size.
	for i, name := range stackSizeLoads {
		fn, ok := stackSizes[name]
		if !ok {
			return fmt.Errorf("could not find symbol %s in ELF file", name)
		}
		if fn.stackSizeType == stacksize.Bounded {
			stackSize := uint32(fn.stackSize)

			// Adding 4 for the stack canary. Even though the size may be
			// automatically determined, stack overflow checking is still
			// important as the stack size cannot be determined for all
			// goroutines.
			stackSize += 4

			// Add stack size used by interrupts.
			switch elfFile.Machine {
			case elf.EM_ARM:
				// On Cortex-M (assumed here), this stack size is 8 words or 32
				// bytes. This is only to store the registers that the interrupt
				// may modify, the interrupt will switch to the interrupt stack
				// (MSP).
				// Some background:
				// https://interrupt.memfault.com/blog/cortex-m-rtos-context-switching
				stackSize += 32
			}

			// Finally write the stack size to the binary.
			binary.LittleEndian.PutUint32(data[i*4:], stackSize)
		}
	}

	// Write back the modified stack sizes.
	_, err = fp.WriteAt(data, int64(section.Offset))
	if err != nil {
		return err
	}

	return nil
}

// printStacks prints the maximum stack depth for functions that are started as
// goroutines. Stack sizes cannot always be determined statically, in particular
// recursive functions and functions that call interface methods or function
// pointers may have an unknown stack depth (depending on what the optimizer
// manages to optimize away).
//
// It might print something like the following:
//
//     function                         stack usage (in bytes)
//     Reset_Handler                    316
//     examples/blinky2.led1            92
//     runtime.run$1                    300
func printStacks(calculatedStacks []string, stackSizes map[string]functionStackSize) {
	// Print the sizes of all stacks.
	fmt.Printf("%-32s %s\n", "function", "stack usage (in bytes)")
	for _, name := range calculatedStacks {
		fn := stackSizes[name]
		switch fn.stackSizeType {
		case stacksize.Bounded:
			fmt.Printf("%-32s %d\n", fn.humanName, fn.stackSize)
		case stacksize.Unknown:
			fmt.Printf("%-32s unknown, %s does not have stack frame information\n", fn.humanName, fn.missingStackSize)
		case stacksize.Recursive:
			fmt.Printf("%-32s recursive, %s may call itself\n", fn.humanName, fn.missingStackSize)
		case stacksize.IndirectCall:
			fmt.Printf("%-32s unknown, %s calls a function pointer\n", fn.humanName, fn.missingStackSize)
		}
	}
}
