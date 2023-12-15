package main

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/rjeczalik/notify"

	_ "embed"
)

func main() {

	var (
		vgutil          = kingpin.New("vgutil", "Vugu assorted utilities")
		verbose         = vgutil.Flag("verbose", "Output more logging info").Short('v').Bool()
		hash            = vgutil.Command("hash", "Compute and print a hash for a file (32-bit FNV-1a)")
		hashIn          = hash.Arg("in", "Input file").Required().String()
		hashRename      = vgutil.Command("hash-rename", "Rename file to include it's hash (32-bit FNV-1a)")
		hashRenameIn    = hashRename.Arg("in", "Input file").Required().String()
		hashRenameOut   = hashRename.Flag("out", "Output file (optional, defaults to rename alongside input file in same dir)").String()
		watch           = vgutil.Command("watch", "Watch a directory for changes")
		watchDirs       = watch.Arg("dirs", "Directories to watch (append /... to make it recursive)").Required().Strings()
		pageTmpl        = vgutil.Command("page-tmpl", "Run page template tool")
		pageTmplIn      = pageTmpl.Flag("in", "Input template file").String()
		pageTmplOut     = pageTmpl.Flag("out", "Output HTML file").String()
		pageTmplTmplOut = pageTmpl.Flag("tmpl-out", "Output default template file to this path (will not overwrite unless --force is specified)").String()
		pageTmplForce   = pageTmpl.Flag("force", "Force overwrite of output template file").Short('f').Bool()
		pageTmplFiles   = pageTmpl.Arg("files", "Files to make the template aware of").Strings()
	)

	switch kingpin.MustParse(vgutil.Parse(os.Args[1:])) {

	// compute hash on a file
	case hash.FullCommand():
		h := fnv.New32a()
		b, err := os.ReadFile(*hashIn)
		if err != nil {
			panic(err)
		}
		h.Write(b)
		fmt.Printf("%x\n", h.Sum(nil))
		return

	// compute hash on file and rename it
	case hashRename.FullCommand():
		inFname := *hashRenameIn
		h := fnv.New32a()
		b, err := os.ReadFile(inFname)
		if err != nil {
			panic(err)
		}
		h.Write(b)
		hhexval := fmt.Sprintf("%x", h.Sum(nil))

		inp := *hashRenameOut
		if inp == "" {
			inp = inFname
		}

		pathPrefix, inBaseName := filepath.Split(inp)
		outNoSlug := stripSlug(inBaseName)
		outExt := filepath.Ext(outNoSlug)
		outBaseNoExt := strings.TrimSuffix(filepath.Base(outNoSlug), outExt)

		outFname := filepath.Join(pathPrefix, fmt.Sprintf("%s-%s%s", outBaseNoExt, hhexval, outExt))

		log.Printf("Renaming %q -> %q", inFname, outFname)
		err = os.Rename(inFname, outFname)
		if err != nil {
			panic(err)
		}

		return

	// wait for a change on any of the indicated directories
	case watch.FullCommand():

		if len(*watchDirs) == 0 {
			log.Fatal("One or more watch directories must be specified")
		}

		done := make(chan struct{}, 1)

		// TODO: This would be even better if it listened for various events
		// and then waited until a certain time period elapsed with no
		// further changes (e.g. 1 or 2 seconds). This would better handle
		// bursts of changes such as find & replace operations on multiple
		// files or any other tooling that might change several files
		// at a time.

		for _, watchDir := range *watchDirs {

			c := make(chan notify.EventInfo, 1)
			if err := notify.Watch(watchDir, c, notify.All); err != nil {
				log.Fatal(err)
			}
			defer notify.Stop(c)

			go func() {
				defer func() { done <- struct{}{} }()
				ei := <-c
				log.Printf("Event: %v\n", ei)
			}()

		}

		// wait for any of the directory change listeners to complete
		<-done

		return

	// run page template
	case pageTmpl.FullCommand():

		var err error

		// tmpl-out option writes the default template out, but does not
		// overwrite, this makes it easy for new projects
		if *pageTmplTmplOut != "" {
			if !*pageTmplForce {
				_, err := os.Stat(*pageTmplTmplOut)
				if err == nil {
					log.Printf("Template file %q already exists, not overwriting", *pageTmplTmplOut)
					return
				}
			}
			if errors.Is(err, fs.ErrNotExist) { // ignore not exist error
				err = nil
			}
			if err != nil { // any other weird error we want to report and exit
				log.Fatal(err)
			}
			err = os.WriteFile(*pageTmplTmplOut, []byte(defaultPageTmpl), 0644)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Write template file %q", *pageTmplTmplOut)
			return
		}

		tmpl := template.New("page")

		type fmapEntry struct {
			name    string    // file name e.g. "whatever-abcd1234.css"
			path    string    // file path as specified on the command line, including dir e.g. "./public/whatever-abcd1234.css"
			modTime time.Time // file modification timestamp
		}
		fmap := make(map[string]fmapEntry, len(*pageTmplFiles))
		for _, fn := range *pageTmplFiles {
			st, err := os.Stat(fn)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					log.Printf("Warning: Skipping missing file %q", fn)
					continue // skip missing files
				}
				log.Fatalf("Error on input file %q: %v", fn, err)
			}
			name := filepath.Base(fn)
			key := stripSlug(name)
			fme := fmap[key]
			// only replace entry if this one is newer, or first time
			if fme.modTime.IsZero() || st.ModTime().After(fme.modTime) {
				fme = fmapEntry{
					name:    name,
					path:    fn,
					modTime: st.ModTime(),
				}
				fmap[key] = fme
			}
		}
		if *verbose {
			log.Printf("fmap after reading inputs: %#v", fmap)
		}

		pageBaseName := "index" // default if no --in param

		tmpl = tmpl.Funcs(template.FuncMap{
			"PageBaseName": func() string {
				return pageBaseName
			},
			"FileName": func(parts ...string) (ret string) {
				key := strings.Join(parts, "")
				if *verbose {
					defer func() { log.Printf("FileName %q returning %q", key, ret) }()
				}
				fme, ok := fmap[key]
				if !ok {
					return ""
				}
				return fme.name
			},
			"FileExists": func(parts ...string) (ret bool) {
				key := strings.Join(parts, "")
				if *verbose {
					defer func() { log.Printf("FileExists %q returning %v", key, ret) }()
				}
				_, ok := fmap[key]
				return ok
			},
		})

		if *pageTmplIn == "" {
			log.Printf("No --in template specified, using default")
			tmpl, err = tmpl.Parse(defaultPageTmpl)
			if err != nil {
				log.Fatal(err)
			}
		} else {
			// convert "somepath/somefile.ext" -> "somefile"
			pageBaseName = strings.TrimSuffix(filepath.Base(*pageTmplIn), filepath.Ext(*pageTmplIn))
			b, err := os.ReadFile(*pageTmplIn)
			if err != nil {
				log.Fatal(err)
			}
			tmpl, err = tmpl.Parse(string(b))
			if err != nil {
				log.Fatal(err)
			}
		}

		// TODO: any other data we want to add? or should we just
		// stick to funcs in the FuncMap?
		var data struct{}

		var outBuf bytes.Buffer
		err = tmpl.ExecuteTemplate(&outBuf, "page", data)
		if err != nil {
			log.Fatal(err)
		}

		if *pageTmplOut == "" {
			log.Fatal("--out output file is required")
		}
		err = os.WriteFile(*pageTmplOut, outBuf.Bytes(), 0644)
		if err != nil {
			log.Fatal(err)
		}

		return

	default:
		log.Fatal("No command specified")
	}

}

var stripSlugRE = regexp.MustCompile(`([_-][a-f0-9]{8}[.])`)

// stripSlug will look for a dash or underscore followed by an 8-hex-byte slug
// and then a period and remove it, e.g. "whatever-abcd1234.css" -> "whatever.css"
func stripSlug(in string) (ret string) {
	ret = stripSlugRE.ReplaceAllString(in, ".")
	// log.Printf("stripSlug %q -> %q", in, ret)
	return ret
}

//go:embed default-page.tmpl
var defaultPageTmpl string
