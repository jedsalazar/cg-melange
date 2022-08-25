// Copyright 2022 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	apkofs "chainguard.dev/apko/pkg/fs"
	"chainguard.dev/apko/pkg/tarball"
	"chainguard.dev/melange/internal/sign"
	"github.com/psanford/memfs"
)

type PackageContext struct {
	Context       *Context
	Origin        *Package
	PackageName   string
	InstalledSize int64
	DataHash      string
	OutDir        string
	Logger        *log.Logger
	Dependencies  Dependencies
	Arch          string
	Options       PackageOption
}

func (pkg *Package) Emit(ctx *PipelineContext) error {
	fakesp := Subpackage{
		Name:         pkg.Name,
		Dependencies: pkg.Dependencies,
		Options:      pkg.Options,
	}
	return fakesp.Emit(ctx)
}

func (spkg *Subpackage) Emit(ctx *PipelineContext) error {
	pc := PackageContext{
		Context:      ctx.Context,
		Origin:       &ctx.Context.Configuration.Package,
		PackageName:  spkg.Name,
		OutDir:       filepath.Join(ctx.Context.OutDir, ctx.Context.Arch.ToAPK()),
		Logger:       log.New(log.Writer(), fmt.Sprintf("melange (%s/%s): ", spkg.Name, ctx.Context.Arch.ToAPK()), log.LstdFlags|log.Lmsgprefix),
		Dependencies: spkg.Dependencies,
		Arch:         ctx.Context.Arch.ToAPK(),
		Options:      spkg.Options,
	}
	return pc.EmitPackage()
}

func (pc *PackageContext) Identity() string {
	return fmt.Sprintf("%s-%s-r%d", pc.PackageName, pc.Origin.Version, pc.Origin.Epoch)
}

func (pc *PackageContext) Filename() string {
	return fmt.Sprintf("%s/%s.apk", pc.OutDir, pc.Identity())
}

func (pc *PackageContext) WorkspaceSubdir() string {
	return filepath.Join(pc.Context.WorkspaceDir, "melange-out", pc.PackageName)
}

var controlTemplate = `# Generated by melange.
pkgname = {{.PackageName}}
pkgver = {{.Origin.Version}}-r{{.Origin.Epoch}}
arch = {{.Arch}}
size = {{.InstalledSize}}
pkgdesc = {{.Origin.Description}}
{{- range $copyright := .Origin.Copyright }}
license = {{ $copyright.License }}
{{- end }}
{{- range $dep := .Dependencies.Runtime }}
depend = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Provides }}
provides = {{ $dep }}
{{- end }}
datahash = {{.DataHash}}
`

func (pc *PackageContext) GenerateControlData(w io.Writer) error {
	tmpl := template.New("control")
	return template.Must(tmpl.Parse(controlTemplate)).Execute(w, pc)
}

func (pc *PackageContext) generateControlSection(digest hash.Hash, w io.WriteSeeker) (hash.Hash, error) {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return digest, fmt.Errorf("unable to build tarball context: %w", err)
	}

	var controlBuf bytes.Buffer
	if err := pc.GenerateControlData(&controlBuf); err != nil {
		return digest, fmt.Errorf("unable to process control template: %w", err)
	}

	fsys := memfs.New()
	if err := fsys.WriteFile(".PKGINFO", controlBuf.Bytes(), 0644); err != nil {
		return digest, fmt.Errorf("unable to build control FS: %w", err)
	}

	mw := io.MultiWriter(digest, w)
	if err := tarctx.WriteArchive(mw, fsys); err != nil {
		return digest, fmt.Errorf("unable to write control tarball: %w", err)
	}

	controlHash := hex.EncodeToString(digest.Sum(nil))
	pc.Logger.Printf("  control.tar.gz digest: %s", controlHash)

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return digest, fmt.Errorf("unable to rewind control tarball: %w", err)
	}

	return digest, nil
}

func (pc *PackageContext) SignatureName() string {
	return fmt.Sprintf(".SIGN.RSA.%s.pub", filepath.Base(pc.Context.SigningKey))
}

type DependencyGenerator func(*PackageContext, *Dependencies) error

func dedup(in []string) []string {
	sort.Strings(in)
	out := make([]string, 0, len(in))

	var prev string
	for _, cur := range in {
		if cur == prev {
			continue
		}
		out = append(out, cur)
		prev = cur
	}

	return out
}

func allowedPrefix(path string, prefixes []string) bool {
	for _, pfx := range prefixes {
		if strings.HasPrefix(path, pfx) {
			return true
		}
	}

	return false
}

var cmdPrefixes = []string{"bin", "sbin", "usr/bin", "usr/sbin"}

func generateCmdProviders(pc *PackageContext, generated *Dependencies) error {
	pc.Logger.Printf("scanning for commands...")

	fsys := apkofs.DirFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm()&0555 == 0555 {
			if allowedPrefix(path, cmdPrefixes) {
				basename := filepath.Base(path)
				generated.Provides = append(generated.Provides, fmt.Sprintf("cmd:%s=%s-r%d", basename, pc.Origin.Version, pc.Origin.Epoch))
			}
		}

		return nil
	}); err != nil {
		return err
	}

	return nil
}

func generateSharedObjectNameDeps(pc *PackageContext, generated *Dependencies) error {
	pc.Logger.Printf("scanning for shared object dependencies...")

	depends := map[string][]string{}

	fsys := apkofs.DirFS(pc.WorkspaceSubdir())
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		mode := fi.Mode()
		if !mode.IsRegular() {
			return nil
		}

		if mode.Perm()&0555 == 0555 {
			basename := filepath.Base(path)

			// most likely a shell script instead of an ELF, so treat any
			// error as non-fatal.
			// TODO(kaniini): use DirFS for this
			ef, err := elf.Open(filepath.Join(pc.WorkspaceSubdir(), path))
			if err != nil {
				return nil
			}
			defer ef.Close()

			libs, err := ef.ImportedLibraries()
			if err != nil {
				pc.Logger.Printf("WTF: ImportedLibraries() returned error: %v", err)
				return nil
			}

			for _, lib := range libs {
				if strings.Contains(lib, ".so.") {
					generated.Runtime = append(generated.Runtime, fmt.Sprintf("so:%s", lib))
					depends[lib] = append(depends[lib], path)
				}
			}

			if strings.Contains(basename, ".so.") {
				sonames, err := ef.DynString(elf.DT_SONAME)
				// most likely SONAME is not set on this object
				if err != nil {
					pc.Logger.Printf("WARNING: library %s lacks SONAME", path)
					return nil
				}

				for _, soname := range sonames {
					parts := strings.Split(soname, ".so.")

					var libver string
					if len(parts) > 1 {
						libver = parts[1]
					} else {
						libver = "0"
					}

					generated.Provides = append(generated.Provides, fmt.Sprintf("so:%s=%s", soname, libver))
				}
			}
		}

		return nil
	}); err != nil {
		return err
	}

	if pc.Context.DependencyLog != "" {
		pc.Logger.Printf("writing dependency log")

		logFile, err := os.Create(fmt.Sprintf("%s.%s", pc.Context.DependencyLog, pc.Arch))
		if err != nil {
			pc.Logger.Printf("WARNING: Unable to open dependency log: %v", err)
		}
		defer logFile.Close()

		je := json.NewEncoder(logFile)
		if err := je.Encode(depends); err != nil {
			return err
		}
	}

	return nil
}

func (dep *Dependencies) Summarize(logger *log.Logger) {
	if len(dep.Runtime) > 0 {
		logger.Printf("  runtime:")

		for _, dep := range dep.Runtime {
			logger.Printf("    %s", dep)
		}
	}

	if len(dep.Provides) > 0 {
		logger.Printf("  provides:")

		for _, dep := range dep.Provides {
			logger.Printf("    %s", dep)
		}
	}
}

func (pc *PackageContext) GenerateDependencies() error {
	generated := Dependencies{}
	generators := []DependencyGenerator{
		generateSharedObjectNameDeps,
		generateCmdProviders,
	}

	for _, gen := range generators {
		if err := gen(pc, &generated); err != nil {
			return err
		}
	}

	newruntime := append(pc.Dependencies.Runtime, generated.Runtime...)
	pc.Dependencies.Runtime = dedup(newruntime)

	newprovides := append(pc.Dependencies.Provides, generated.Provides...)
	pc.Dependencies.Provides = dedup(newprovides)

	pc.Dependencies.Summarize(pc.Logger)

	return nil
}

func combine(out io.Writer, inputs ...io.Reader) error {
	for _, input := range inputs {
		if _, err := io.Copy(out, input); err != nil {
			return err
		}
	}

	return nil
}

// TODO(kaniini): generate APKv3 packages
func (pc *PackageContext) calculateInstalledSize(fsys fs.FS) error {
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		pc.InstalledSize += fi.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("unable to preprocess package data: %w", err)
	}

	return nil
}

func (pc *PackageContext) emitDataSection(fsys fs.FS, w io.WriteSeeker) error {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithUseChecksums(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	digest := sha256.New()
	mw := io.MultiWriter(digest, w)
	if err := tarctx.WriteArchive(mw, fsys); err != nil {
		return fmt.Errorf("unable to write data tarball: %w", err)
	}

	pc.DataHash = hex.EncodeToString(digest.Sum(nil))
	pc.Logger.Printf("  data.tar.gz digest: %s", pc.DataHash)

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind data tarball: %w", err)
	}

	return nil
}

func (pc *PackageContext) emitNormalSignatureSection(h hash.Hash, w io.WriteSeeker) error {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Context.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	fsys := memfs.New()
	sigbuf, err := sign.RSASignSHA1Digest(h.Sum(nil), pc.Context.SigningKey, pc.Context.SigningPassphrase)
	if err != nil {
		return fmt.Errorf("unable to generate signature: %w", err)
	}

	if err := fsys.WriteFile(pc.SignatureName(), sigbuf, 0644); err != nil {
		return fmt.Errorf("unable to build signature FS: %w", err)
	}

	if err := tarctx.WriteArchive(w, fsys); err != nil {
		return fmt.Errorf("unable to write signature tarball: %w", err)
	}

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind signature tarball: %w", err)
	}

	return nil
}

func (pc *PackageContext) wantSignature() bool {
	return pc.Context.SigningKey != ""
}

func (pc *PackageContext) EmitPackage() error {
	pc.Logger.Printf("generating package %s", pc.Identity())

	// filesystem for the data package
	fsys := apkofs.DirFS(pc.WorkspaceSubdir())

	// generate so:/cmd: virtuals for the filesystem
	if err := pc.GenerateDependencies(); err != nil {
		return fmt.Errorf("unable to build final dependencies set: %w", err)
	}

	// walk the filesystem to calculate the installed-size
	if err := pc.calculateInstalledSize(fsys); err != nil {
		return err
	}

	pc.Logger.Printf("  installed-size: %d", pc.InstalledSize)

	// prepare data.tar.gz
	dataTarGz, err := os.CreateTemp("", "melange-data-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer dataTarGz.Close()
	defer os.Remove(dataTarGz.Name())

	if err := pc.emitDataSection(fsys, dataTarGz); err != nil {
		return err
	}

	// prepare control.tar.gz
	controlTarGz, err := os.CreateTemp("", "melange-control-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer controlTarGz.Close()
	defer os.Remove(controlTarGz.Name())

	var controlDigest hash.Hash

	// APKv2 style signature is a SHA-1 hash on the control digest,
	// APKv2+Fulcio style signature is an SHA-256 hash on the control
	// digest.
	controlDigest = sha256.New()

	// Key-based signature (normal), use SHA-1
	if pc.Context.SigningKey != "" {
		controlDigest = sha1.New()
	}

	finalDigest, err := pc.generateControlSection(controlDigest, controlTarGz)
	if err != nil {
		return err
	}

	combinedParts := []io.Reader{controlTarGz, dataTarGz}

	if pc.wantSignature() {
		signatureTarGz, err := os.CreateTemp("", "melange-signature-*.tar.gz")
		if err != nil {
			return fmt.Errorf("unable to open temporary file for writing: %w", err)
		}
		defer signatureTarGz.Close()
		defer os.Remove(signatureTarGz.Name())

		// TODO(kaniini): Emit fulcio signature if signing key not configured.
		if err := pc.emitNormalSignatureSection(finalDigest, signatureTarGz); err != nil {
			return err
		}

		combinedParts = append([]io.Reader{signatureTarGz}, combinedParts...)
	}

	// build the final tarball
	if err := os.MkdirAll(pc.OutDir, 0755); err != nil {
		return fmt.Errorf("unable to create output directory: %w", err)
	}

	outFile, err := os.Create(pc.Filename())
	if err != nil {
		return fmt.Errorf("unable to create apk file: %w", err)
	}
	defer outFile.Close()

	if err := combine(outFile, combinedParts...); err != nil {
		return fmt.Errorf("unable to write apk file: %w", err)
	}

	pc.Logger.Printf("wrote %s", outFile.Name())

	return nil
}
