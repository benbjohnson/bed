package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh/terminal"
)

func main() {
	if err := Run(os.Args[1:]); err == flag.ErrHelp {
		os.Exit(1)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func Run(args []string) error {
	// Parse command line flags.
	fs := flag.NewFlagSet("bed", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "")
	verbose := fs.Bool("v", false, "")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	} else if fs.NArg() == 0 {
		fs.Usage()
		return flag.ErrHelp
	}

	// Ensure either STDIN or args specify paths.
	if terminal.IsTerminal(int(os.Stdin.Fd())) && fs.NArg() == 1 {
		return errors.New("path required")
	}

	// Set logging.
	log.SetFlags(0)
	if !*verbose {
		log.SetOutput(ioutil.Discard)
	}

	// Ensure BED_EDITOR or EDITOR is set.
	editor := os.Getenv("BED_EDITOR")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" && !*dryRun {
		return errors.New("EDITOR must be set")
	}

	// Extract arguments.
	pattern, paths := fs.Arg(0), fs.Args()[1:]

	// Read paths from stdin as well.
	if !terminal.IsTerminal(int(os.Stdin.Fd())) {
		buf, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		paths = append(paths, strings.Split(strings.TrimSpace(string(buf)), "\n")...)
	}

	// Parse regex.
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}

	// Find all matches.
	matches, err := FindAllIndexPaths(re, paths)
	if err != nil {
		return err
	}

	// If a dry run, simply print out matches to STDOUT.
	if *dryRun {
		for _, m := range matches {
			fmt.Printf("%s: %s\n", m.Path, string(m.Data))
		}
		return nil
	}

	// Write matches to temporary file.
	tmpPath, err := writeTempMatchFile(matches)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	// Invoke editor.
	cmd, args := parseEditor(editor)
	if err := exec.Command(cmd, append(args, tmpPath)...).Run(); err != nil {
		return fmt.Errorf("There was a problem with editor %q", editor)
	}

	// Parse matches from file.
	var newMatches []*Match
	if buf, err := ioutil.ReadFile(tmpPath); err != nil {
		return err
	} else if newMatches, err = ParseMatches(buf); err != nil {
		return err
	}

	// Apply changes.
	if err := ApplyMatches(newMatches); err != nil {
		return err
	}

	return nil
}

func parseEditor(s string) (cmd string, args []string) {
	a := strings.Split(s, " ")
	return a[0], a[1:]
}

func writeTempMatchFile(matches []*Match) (string, error) {
	f, err := ioutil.TempFile("", "bed-")
	if err != nil {
		return "", err
	}
	defer f.Close()

	for _, m := range matches {
		if buf, err := m.MarshalText(); err != nil {
			return "", err
		} else if _, err := f.Write(buf); err != nil {
			return "", err
		} else if _, err := f.Write([]byte("\n")); err != nil {
			return "", err
		}
	}
	return f.Name(), nil
}

// FindAllIndexPath finds the start/end position & data of re in all paths.
func FindAllIndexPaths(re *regexp.Regexp, paths []string) ([]*Match, error) {
	var matches []*Match
	for _, path := range paths {
		m, err := FindAllIndexPath(re, path)
		if err != nil {
			return nil, err
		}
		matches = append(matches, m...)
	}
	return matches, nil
}

// FindAllIndexPath finds the start/end position & data of re in path.
func FindAllIndexPath(re *regexp.Regexp, path string) ([]*Match, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	a := re.FindAllIndex(data, -1)
	b := re.FindAll(data, -1)

	var matches []*Match
	for i := range a {
		matches = append(matches, &Match{
			Path: path,
			Pos:  a[i][0],
			Len:  a[i][1] - a[i][0],
			Data: b[i],
		})
	}

	return matches, nil
}

// Match contains the source & position of a match.
type Match struct {
	Path string
	Pos  int
	Len  int
	Data []byte
}

type matchJSON struct {
	Path string `json:"path"`
	Pos  int    `json:"pos"`
	Len  int    `json:"len"`
}

func (m *Match) MarshalText() ([]byte, error) {
	hdr, err := json.Marshal(matchJSON{Path: m.Path, Pos: m.Pos, Len: m.Len})
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "#bed:begin %s\n", hdr)
	fmt.Fprintln(&buf, string(m.Data))
	fmt.Fprintln(&buf, "#bed:end")
	return buf.Bytes(), nil
}

func (m *Match) UnmarshalText(data []byte) error {
	a := matchTextRegex.FindSubmatch(data)
	if len(a) == 0 {
		return errors.New("missing #bed:begin or #bed:end tags")
	}

	var hdr matchJSON
	if err := json.Unmarshal(a[1], &hdr); err != nil {
		return err
	}
	m.Path, m.Pos, m.Len = hdr.Path, hdr.Pos, hdr.Len
	m.Data = a[2]
	return nil
}

var matchTextRegex = regexp.MustCompile(`(?s)#bed:begin ([^\n]+)\n(.*?)\n#bed:end`)

// ParseMatches finds and parses all matches.
// An error is returned if match header data is not a valid header.
func ParseMatches(data []byte) ([]*Match, error) {
	var matches []*Match
	for _, buf := range matchTextRegex.FindAll(data, -1) {
		var m Match
		if err := m.UnmarshalText(buf); err != nil {
			return nil, err
		}
		matches = append(matches, &m)
	}
	return matches, nil
}

// ApplyMatches writes each match's data to the specified path & position.
func ApplyMatches(matches []*Match) error {
	paths, pathMatches := groupMatchesByPath(matches)
	for i := range paths {
		if err := applyPathMatches(paths[i], pathMatches[i]); err != nil {
			return err
		}
	}
	return nil
}

func applyPathMatches(path string, matches []*Match) error {
	// Read current file data.
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}

	// Apply matches in order.
	for i, m := range matches {
		start, end := m.Pos, m.Pos+m.Len

		prefix := data[:start:start]
		mid := m.Data[:len(m.Data):len(m.Data)]
		suffix := data[end:]

		data = append(prefix, append(mid, suffix...)...)

		// Apply difference in data size to later matches.
		for j := i + 1; j < len(matches); j++ {
			if matches[j].Pos >= m.Pos {
				matches[j].Pos += len(m.Data) - m.Len
			}
		}
	}

	// Write new data back to file.
	if fi, err := os.Stat(path); err != nil {
		return err
	} else if err := ioutil.WriteFile(path, data, fi.Mode()); err != nil {
		return err
	}
	return nil
}

// groupMatchesByPath returns a list of paths and a list of their associated matches.
func groupMatchesByPath(matches []*Match) ([]string, [][]*Match) {
	m := make(map[string][]*Match)
	for i := range matches {
		m[matches[i].Path] = append(m[matches[i].Path], matches[i])
	}

	paths, pathMatches := make([]string, 0, len(m)), make([][]*Match, 0, len(m))
	for path := range m {
		paths = append(paths, path)
		pathMatches = append(pathMatches, m[path])
	}
	return paths, pathMatches
}

func usage() {
	fmt.Fprintln(os.Stderr, `
bed is a bulk command line text editor.

Usage:

	bed [arguments] pattern path [paths]

The command will match pattern against all provided paths and output
a series of files which contain matches. This list of matches can be
passed to an interactive editor such as vi for edits. If the editor
is closed with a 0 exit code then all changes to the matches are
applied to the original files.

Available arguments:

	-dry-run
		Only show matches without outputting to files.
`)
}
