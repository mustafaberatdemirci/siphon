package extract

import (
	"path/filepath"
	"sort"
)

// Archive is one archive among files: First is what Extract is given, Parts
// every file of it (First among them; a whole archive is its own only part).
type Archive struct {
	First string
	Parts []string
}

// Archives picks the archives out of paths (finished downloads): whole
// ones each on their own, parts gathered by folder and set. A set whose
// first part isn't among paths is left out; one missing a later part is
// not, and 7-Zip says so when it unpacks. They come in paths' order of
// their first part.
func Archives(paths []string) []Archive {
	type key struct{ dir, set string }
	sets := map[key]*Archive{}
	var out []*Archive
	for _, p := range paths {
		kind, part := Of(filepath.Base(p))
		if kind == None {
			continue
		}
		if part.Set == "" {
			out = append(out, &Archive{First: p, Parts: []string{p}})
			continue
		}
		k := key{filepath.Clean(filepath.Dir(p)), part.Set}
		a := sets[k]
		if a == nil {
			a = &Archive{}
			sets[k] = a
			out = append(out, a)
		}
		a.Parts = append(a.Parts, p)
		if part.Number == 1 {
			a.First = p
		}
	}
	res := make([]Archive, 0, len(out))
	for _, a := range out {
		if a.First != "" {
			res = append(res, *a)
		}
	}
	sort.SliceStable(res, func(i, j int) bool { return index(paths, res[i].First) < index(paths, res[j].First) })
	return res
}

func index(paths []string, p string) int {
	for i, q := range paths {
		if q == p {
			return i
		}
	}
	return len(paths)
}
