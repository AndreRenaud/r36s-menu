package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Entry is one line of the menu: a ROM, or a folder with ROMs somewhere
// inside it.
type Entry struct {
	Name     string // file or folder name
	Title    string // what the menu shows
	Path     string
	System   System   // SysNone for a folder
	Children []*Entry // a folder's entries
	Shot     string   // screenshot path, "" if there is none
	Size     int64
	Parent   *Entry
}

func (e *Entry) IsDir() bool { return e.System == SysNone }

// Count is the number of ROMs in a folder and its sub-folders.
func (e *Entry) Count() int {
	if !e.IsDir() {
		return 1
	}
	n := 0
	for _, c := range e.Children {
		n += c.Count()
	}
	return n
}

var imageExts = []string{".png", ".jpg", ".jpeg"}

var tagRE = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]`)

// cleanTitle turns a file name into a title: no extension, no "(USA)" or
// "[!]" tags, underscores as spaces.
func cleanTitle(name string, dir bool) string {
	t := name
	if !dir {
		t = strings.TrimSuffix(t, filepath.Ext(t))
	}
	if s := strings.TrimSpace(tagRE.ReplaceAllString(t, "")); s != "" {
		t = s
	}
	t = strings.ReplaceAll(t, "_", " ")
	return strings.TrimSpace(t)
}

// Scan reads the folder tree at root. ROMs are kept, everything else is
// hidden, and so are folders without ROMs. Folders come first, then ROMs,
// each in alphabetical order.
func Scan(root string) *Entry {
	top := &Entry{Name: filepath.Base(root), Path: root} // the top level has no title
	scanDir(top, 0)
	return top
}

func scanDir(dir *Entry, depth int) {
	items, err := os.ReadDir(dir.Path)
	if err != nil || depth > 8 {
		return
	}
	// Screenshots are named after the ROM: "Sonic.md" has "Sonic.png"
	shots := map[string]string{}
	cues := map[string]bool{}
	for _, it := range items {
		ext := strings.ToLower(filepath.Ext(it.Name()))
		if ext == ".cue" {
			cues[strings.ToLower(strings.TrimSuffix(it.Name(), filepath.Ext(it.Name())))] = true
		}
		for _, ie := range imageExts {
			if ext == ie {
				shots[strings.ToLower(strings.TrimSuffix(it.Name(), filepath.Ext(it.Name())))] = filepath.Join(dir.Path, it.Name())
			}
		}
	}
	for _, it := range items {
		name := it.Name()
		if strings.HasPrefix(name, ".") || name == "System Volume Information" {
			continue
		}
		path := filepath.Join(dir.Path, name)
		if it.IsDir() {
			if strings.EqualFold(name, "bios") {
				continue
			}
			sub := &Entry{Name: name, Title: cleanTitle(name, true), Path: path, Parent: dir}
			scanDir(sub, depth+1)
			if len(sub.Children) > 0 {
				dir.Children = append(dir.Children, sub)
			}
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".cue" && cues[strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))] {
			continue // a disc image's data: its .cue is the entry
		}
		sys := DetectFile(path)
		if sys == SysNone || isBIOS(name) {
			continue
		}
		e := &Entry{Name: name, Title: cleanTitle(name, false), Path: path, System: sys, Parent: dir}
		if info, err := it.Info(); err == nil {
			e.Size = info.Size()
		}
		e.Shot = shots[strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))]
		dir.Children = append(dir.Children, e)
	}
	// Where dropping the tags makes two titles the same ("Game (USA)" and
	// "Game (Japan)"), keep them.
	seen := map[string]int{}
	for _, c := range dir.Children {
		seen[strings.ToLower(c.Title)]++
	}
	for _, c := range dir.Children {
		if seen[strings.ToLower(c.Title)] > 1 && !c.IsDir() {
			c.Title = strings.ReplaceAll(strings.TrimSuffix(c.Name, filepath.Ext(c.Name)), "_", " ")
		}
	}
	sort.SliceStable(dir.Children, func(i, j int) bool {
		a, b := dir.Children[i], dir.Children[j]
		if a.IsDir() != b.IsDir() {
			return a.IsDir()
		}
		return strings.ToLower(a.Title) < strings.ToLower(b.Title)
	})
}

// Arcade BIOS sets sit next to the games but aren't games.
var biosSets = map[string]bool{
	"neogeo.zip": true, "pgm.zip": true, "decocass.zip": true, "cvs.zip": true,
	"skns.zip": true, "nmk004.zip": true, "isgsm.zip": true, "midssio.zip": true,
	"ym2608.zip": true, "namcoc69.zip": true, "namcoc70.zip": true, "namcoc75.zip": true,
}

func isBIOS(name string) bool { return biosSets[strings.ToLower(name)] }
