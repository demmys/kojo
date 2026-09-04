package extpkg

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// UI locale contributions.
//
// The web bundle is built ahead of time and embedded in the binary, so
// a package cannot ship React — but a translation is data, not code,
// and that is a shape the prebuilt bundle can consume. A package
// declares contributes.locales; the frontend fetches the catalogue at
// boot and overlays it on the compiled-in English strings.
//
// Unlike skills and MCP servers there is no per-agent binding here: the
// UI language is a property of the browser, not of an agent, so global
// enablement is the only gate.

// localeFileMax caps a single translation file. Catalogues are a few
// hundred short strings; anything past this is a mistake or an attempt
// to make the server read a huge file into memory on an unauthenticated
// path's behalf.
const localeFileMax = 1 << 20

// LocaleContribution is one language offered by an installed package.
//
// Which package supplied it is deliberately absent: the list is readable
// by any dashboard client, and the frontend only needs a tag and an
// endonym for the picker. Naming the package would tell an unauthenticated
// viewer what is installed on the instance for no benefit.
type LocaleContribution struct {
	Tag  string `json:"tag"`
	Name string `json:"name"`
}

// Locales lists the languages contributed by every enabled package.
//
// Two packages claiming the same tag is resolved by package ID order
// rather than reported as an error: the registry is assembled one
// install at a time and has no moment at which it could reject the
// collision, and a language picker with a duplicate entry is worse than
// an arbitrary but stable winner.
func (m *Manager) Locales() []LocaleContribution {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []LocaleContribution
	seen := map[string]bool{}
	for _, id := range m.st.sortedIDs() {
		row := m.st.Extensions[id]
		if !row.Enabled {
			continue
		}
		for _, loc := range row.Manifest.Contributes.Locales {
			if seen[loc.Tag] {
				continue
			}
			seen[loc.Tag] = true
			out = append(out, LocaleContribution{Tag: loc.Tag, Name: loc.Name})
		}
	}
	return out
}

// LocaleMessages returns the message catalogue for one tag, merged
// across every enabled package that contributes it. Earlier packages in
// ID order win per key, matching the winner Locales reports.
func (m *Manager) LocaleMessages(tag string) (map[string]string, error) {
	if !localeTagRe.MatchString(tag) {
		return nil, fmt.Errorf("invalid locale tag %q", tag)
	}
	type job struct{ id, file string }
	m.mu.Lock()
	var jobs []job
	for _, id := range m.st.sortedIDs() {
		row := m.st.Extensions[id]
		if !row.Enabled {
			continue
		}
		for _, loc := range row.Manifest.Contributes.Locales {
			if loc.Tag == tag {
				jobs = append(jobs, job{id: id, file: loc.File})
			}
		}
	}
	root := m.root
	m.mu.Unlock()

	if len(jobs) == 0 {
		return nil, fmt.Errorf("%w: locale %s", ErrNotFound, tag)
	}
	out := map[string]string{}
	for _, j := range jobs {
		msgs, err := readPackageLocale(pkgPath(root, j.id), j.file)
		if err != nil {
			// One broken package must not blank the language for
			// the others contributing to it.
			m.logger.Warn("extension locale unreadable",
				"extension", j.id, "tag", tag, "err", err)
			continue
		}
		for k, v := range msgs {
			if _, taken := out[k]; !taken {
				out[k] = v
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: locale %s", ErrNotFound, tag)
	}
	return out, nil
}

// readPackageLocale resolves a manifest-declared catalogue path inside
// a checkout and reads it.
//
// The path goes through containedPath rather than being trusted from
// install time: symlinks inside a checkout can be swapped afterwards,
// and this read happens on every page load, long after
// verifyContributions ran.
func readPackageLocale(pkgDir, rel string) (map[string]string, error) {
	path, err := containedPath(pkgDir, rel)
	if err != nil {
		return nil, err
	}
	return readLocaleFile(path)
}

// readLocaleFile decodes a translation file into a flat key→string map.
//
// Non-string values are dropped rather than rejected: the frontend
// interpolates every value as a string, so a number or an object would
// surface as "[object Object]" in the UI, and the rest of the file is
// still perfectly usable.
func readLocaleFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Size is checked from the open handle rather than by path, so
	// the file measured is the file read. LimitReader still bounds
	// the decode: the size check is a courtesy that gives a clear
	// error, not the thing that makes the read safe.
	if st, err := f.Stat(); err == nil && st.Size() > localeFileMax {
		return nil, fmt.Errorf("locale file is %d bytes, over the %d limit", st.Size(), localeFileMax)
	}
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(io.LimitReader(f, localeFileMax))
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	// A catalogue is one JSON object and nothing else. Decode alone
	// would accept `null` (a nil map), and would stop at the end of
	// the first value in a file holding several — either way the
	// package installs and then serves a language that renders
	// nothing.
	if raw == nil {
		return nil, fmt.Errorf("locale file is not a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("locale file has trailing content after the object")
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		// Unmarshalling JSON null into a string succeeds and leaves it
		// empty, which would register a blank translation and hide the
		// English fallback. Treat null as "not translated".
		if string(v) == "null" {
			continue
		}
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("locale file contains no translated strings")
	}
	return out, nil
}
