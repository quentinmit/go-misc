// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/sha1"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	verbose = flag.Bool("v", false, "print commands being run")
	verDir  = flag.String("dir", defaultVerDir(), "`directory` of saved Go roots")
)

var goroot = runtime.GOROOT()

var binTools = []string{"go", "godoc", "gofmt"}

func defaultVerDir() string {
	cache := os.Getenv("XDG_CACHE_HOME")
	if cache == "" {
		home := os.Getenv("HOME")
		if home == "" {
			u, err := user.Current()
			if err != nil {
				home = u.HomeDir
			}
		}
		cache = filepath.Join(home, ".cache")
	}
	return filepath.Join(cache, "gover")
}

func main() {
	log.SetFlags(0)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  %s [flags] save [name]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s [flags] list\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s [flags] run name command...\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		flag.PrintDefaults()
	}

	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	switch flag.Arg(0) {
	case "save":
		if flag.NArg() > 2 {
			flag.Usage()
			os.Exit(2)
		}
		hash, diff := getHash()
		name := ""
		if flag.NArg() >= 2 {
			name = flag.Arg(1)
		}
		doSave(name, hash, diff)

	case "list":
		if flag.NArg() > 1 {
			flag.Usage()
			os.Exit(2)
		}
		doList()

	case "run":
		if flag.NArg() < 3 {
			flag.Usage()
			os.Exit(2)
		}
		doRun(flag.Arg(1), flag.Args()[2:])

	default:
		flag.Usage()
		os.Exit(2)
	}
}

func gitCmd(cmd string, args ...string) string {
	args = append([]string{"-C", goroot, cmd}, args...)
	c := exec.Command("git", args...)
	c.Stderr = os.Stderr
	output, err := c.Output()
	if err != nil {
		log.Fatal("error executing git %s: %s", strings.Join(args, " "), err)
	}
	return string(output)
}

func getHash() (string, []byte) {
	rev := strings.TrimSpace(string(gitCmd("rev-parse", "--short", "HEAD")))

	diff := []byte(gitCmd("diff", "HEAD"))

	if len(bytes.TrimSpace(diff)) > 0 {
		diffHash := fmt.Sprintf("%x", sha1.Sum(diff))
		return rev + "+" + diffHash[:10], diff
	}
	return rev, nil
}

func doSave(name string, hash string, diff []byte) {
	// Create a minimal GOROOT at $GOROOT/gover/hash.
	savePath := filepath.Join(*verDir, hash)
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if x := os.Getenv("GOOS"); x != "" {
		goos = x
	}
	if x := os.Getenv("GOARCH"); x != "" {
		goarch = x
	}
	osArch := goos + "_" + goarch

	for _, binTool := range binTools {
		src := filepath.Join(goroot, "bin", binTool)
		if _, err := os.Stat(src); err == nil {
			cp(src, filepath.Join(savePath, "bin", binTool))
		}
	}
	cpR(filepath.Join(goroot, "pkg", osArch), filepath.Join(savePath, "pkg", osArch))
	cpR(filepath.Join(goroot, "pkg", "tool", osArch), filepath.Join(savePath, "pkg", "tool", osArch))
	cpR(filepath.Join(goroot, "pkg", "include"), filepath.Join(savePath, "pkg", "include"))
	cpR(filepath.Join(goroot, "src"), filepath.Join(savePath, "src"))

	if diff != nil {
		if err := ioutil.WriteFile(filepath.Join(savePath, "diff"), diff, 0666); err != nil {
			log.Fatal(err)
		}
	}

	// Save commit object.
	commit := gitCmd("cat-file", "commit", "HEAD")
	if err := ioutil.WriteFile(filepath.Join(savePath, "commit"), []byte(commit), 0666); err != nil {
		log.Fatal(err)
	}

	// If there's a name, symlink it under that name.
	if name != "" && name != hash {
		err := os.Symlink(hash, filepath.Join(*verDir, name))
		if err != nil {
			log.Fatal(err)
		}
	}
}

type commit struct {
	authorDate time.Time
	topLine    string
}

func parseCommit(obj []byte) commit {
	out := commit{}
	lines := strings.Split(string(obj), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "author ") {
			fs := strings.Fields(line)
			secs, err := strconv.ParseInt(fs[len(fs)-2], 10, 64)
			if err != nil {
				log.Fatal("malformed author in commit: %s", err)
			}
			out.authorDate = time.Unix(secs, 0)
		}
		if len(line) == 0 {
			out.topLine = lines[i+1]
			break
		}
	}
	return out
}

type saveInfo struct {
	base   string
	names  []string
	commit commit
}

type saveInfoSorter []*saveInfo

func (s saveInfoSorter) Len() int {
	return len(s)
}

func (s saveInfoSorter) Less(i, j int) bool {
	return s[i].commit.authorDate.Before(s[j].commit.authorDate)
}

func (s saveInfoSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func doList() {
	files, err := ioutil.ReadDir(*verDir)
	if os.IsNotExist(err) {
		return
	} else if err != nil {
		log.Fatal(err)
	}

	baseMap := make(map[string]*saveInfo)
	bases := []*saveInfo{}
	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		info := &saveInfo{base: file.Name(), names: []string{}}
		baseMap[file.Name()] = info
		bases = append(bases, info)

		commit, err := ioutil.ReadFile(filepath.Join(*verDir, file.Name(), "commit"))
		if os.IsNotExist(err) {
			continue
		}
		info.commit = parseCommit(commit)
	}
	for _, file := range files {
		if file.Mode()&os.ModeType == os.ModeSymlink {
			base, err := os.Readlink(filepath.Join(*verDir, file.Name()))
			if err != nil {
				continue
			}
			if info, ok := baseMap[base]; ok {
				info.names = append(info.names, file.Name())
			}
		}
	}

	sort.Sort(saveInfoSorter(bases))

	for _, info := range bases {
		fmt.Print(info.base)
		if !info.commit.authorDate.IsZero() {
			fmt.Printf(" %s", info.commit.authorDate.Local().Format("2006-01-02T15:04:05"))
		}
		if len(info.names) > 0 {
			fmt.Printf(" %s", info.names)
		}
		if info.commit.topLine != "" {
			fmt.Printf(" %s", info.commit.topLine)
		}
		fmt.Println()
	}
}

func doRun(name string, cmd []string) {
	savePath := filepath.Join(*verDir, name)

	c := exec.Command(filepath.Join(savePath, "bin", cmd[0]), cmd[1:]...)
	c.Env = append([]string(nil), os.Environ()...)
	c.Env = append(c.Env, "GOROOT="+savePath)

	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Printf("command failed: %s\n", err)
		os.Exit(1)
	}
}

func cp(src, dst string) {
	if *verbose {
		fmt.Printf("cp %s %s\n", src, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0777); err != nil {
		log.Fatal(err)
	}
	data, err := ioutil.ReadFile(src)
	if err != nil {
		log.Fatal(err)
	}
	st, err := os.Stat(src)
	if err != nil {
		log.Fatal(err)
	}
	if err := ioutil.WriteFile(dst, data, st.Mode()); err != nil {
		log.Fatal(err)
	}
	if err := os.Chtimes(dst, st.ModTime(), st.ModTime()); err != nil {
		log.Fatal(err)
	}
}

func cpR(src, dst string) {
	filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if base == "core" || strings.HasSuffix(base, ".test") {
			return nil
		}

		cp(path, dst+path[len(src):])
		return nil
	})
}
