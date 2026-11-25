package engine

import (
	"debug/elf"
	"regexp"
)

// engineVersionRE matches the client version string the engine embeds in its
// read-only data, e.g. "2.748.0.1527". The third component is always 0 in
// shipped builds; the fourth is the numeric build. Variants such as
// "2.748.0.H3" (a hash-suffixed channel tag) are deliberately not matched.
var engineVersionRE = regexp.MustCompile(`\x00(2\.[0-9]{3}\.0\.[0-9]+)\x00`)

// EngineVersion returns the Roblox client version embedded in the image (for
// example "2.748.0.1527"), or "" if none is found. Image-specific offsets in
// the runtime are keyed by this string, so an unknown build gets none of them.
func (b *Binary) EngineVersion() string {
	for _, name := range []string{".rodata", ".rodata.str1.1"} {
		s := b.File.Section(name)
		if s == nil || s.Type == elf.SHT_NOBITS {
			continue
		}
		data, err := s.Data()
		if err != nil {
			continue
		}
		if m := engineVersionRE.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return ""
}
