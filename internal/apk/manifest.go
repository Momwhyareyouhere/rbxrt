package apk

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
)

// Android binary XML (AXML) chunk types.
const (
	chunkStringPool     = 0x0001
	chunkXML            = 0x0003
	chunkXMLResourceMap = 0x0180
	chunkStartNS        = 0x0100
	chunkEndNS          = 0x0101
	chunkStartElement   = 0x0102
	chunkEndElement     = 0x0103
	chunkCDATA          = 0x0104
)

// Android attribute data types (subset we care about).
const (
	typeNull       = 0x00
	typeString     = 0x03
	typeIntDec     = 0x10
	typeIntHex     = 0x11
	typeIntBoolean = 0x12
)

// Well-known android: attribute resource IDs.
const (
	resVersionCode = 0x0101021b
	resVersionName = 0x0101021c
	resMinSDK      = 0x0101020c
	resTargetSDK   = 0x01010270
)

// Manifest is the subset of AndroidManifest.xml the runtime needs.
type Manifest struct {
	Package     string
	VersionCode int64
	VersionName string
	MinSDK      int64
	TargetSDK   int64
	// Elements retains every start element (name + attributes) for auditing.
	Elements []ManifestElement
}

// ManifestElement is one XML element with resolved attributes.
type ManifestElement struct {
	Name  string
	Attrs []ManifestAttr
}

// ManifestAttr is one resolved attribute.
type ManifestAttr struct {
	NS    string
	Name  string
	Value string
	Raw   string
	ResID uint32
	Type  uint8
	Int   int64
}

type stringPool struct {
	utf8    bool
	strings []string
}

func (p *stringPool) at(i uint32) string {
	if int(i) < len(p.strings) {
		return p.strings[i]
	}
	return ""
}

// ParseManifest decodes a binary AndroidManifest.xml.
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("axml: too short")
	}
	typ := binary.LittleEndian.Uint16(data[0:])
	if typ != chunkXML {
		return nil, fmt.Errorf("axml: not a binary XML document (type 0x%04x)", typ)
	}
	hdr := int(binary.LittleEndian.Uint16(data[2:]))
	total := int(binary.LittleEndian.Uint32(data[4:]))
	if total > len(data) {
		total = len(data)
	}

	pool := &stringPool{}
	var resIDs []uint32
	m := &Manifest{VersionCode: -1, MinSDK: -1, TargetSDK: -1}

	off := hdr
	for off+8 <= total {
		ctype := binary.LittleEndian.Uint16(data[off:])
		chdr := int(binary.LittleEndian.Uint16(data[off+2:]))
		csize := int(binary.LittleEndian.Uint32(data[off+4:]))
		if csize < 8 || off+csize > total {
			break
		}
		body := data[off : off+csize]
		switch ctype {
		case chunkStringPool:
			p, err := parseStringPool(body)
			if err != nil {
				return nil, err
			}
			pool = p
		case chunkXMLResourceMap:
			n := (len(body) - chdr) / 4
			for i := 0; i < n; i++ {
				resIDs = append(resIDs, binary.LittleEndian.Uint32(body[chdr+i*4:]))
			}
		case chunkStartElement:
			e, err := parseStartElement(body, pool, resIDs)
			if err != nil {
				return nil, err
			}
			m.Elements = append(m.Elements, e)
			applyManifestElement(m, e)
		}
		off += csize
	}
	return m, nil
}

func parseStringPool(b []byte) (*stringPool, error) {
	if len(b) < 28 {
		return nil, fmt.Errorf("axml: string pool too short")
	}
	count := int(binary.LittleEndian.Uint32(b[8:]))
	flags := binary.LittleEndian.Uint32(b[16:])
	stringsStart := int(binary.LittleEndian.Uint32(b[20:]))
	utf8 := flags&(1<<8) != 0

	p := &stringPool{utf8: utf8}
	if count == 0 {
		return p, nil
	}
	if stringsStart <= 0 || stringsStart > len(b) {
		return nil, fmt.Errorf("axml: bad strings offset")
	}
	p.strings = make([]string, 0, count)
	data := b[stringsStart:]
	for i := 0; i < count; i++ {
		offPos := 28 + i*4
		if offPos+4 > len(b) {
			break
		}
		so := int(binary.LittleEndian.Uint32(b[offPos:]))
		if so < 0 || so > len(data) {
			p.strings = append(p.strings, "")
			continue
		}
		if utf8 {
			p.strings = append(p.strings, decodeUTF8String(data[so:]))
		} else {
			p.strings = append(p.strings, decodeUTF16String(data[so:]))
		}
	}
	return p, nil
}

// decodeUTF8String decodes one entry in a UTF-8 string pool. Format: two
// varint lengths — UTF-8 byte count, then UTF-16 code-unit count — followed by
// the UTF-8 bytes (no NUL terminator). Mirrors AOSP decodeLength()/string8().
func decodeUTF8String(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	u8len, i := decodeLength(b, 0)
	_, i2 := decodeLength(b, i)
	if u8len < 0 || i2 < 0 {
		return ""
	}
	i += i2
	end := i + u8len
	if end > len(b) {
		end = len(b)
	}
	if i == end {
		return ""
	}
	return string(b[i:end])
}

// decodeLength reads the one-or-two byte length varint used by the string pool
// ("high bit set means a second byte follows"): n = (b[0]&0x7f)<<8 | b[1].
func decodeLength(b []byte, i int) (int, int) {
	if i >= len(b) {
		return -1, -1
	}
	n := int(b[i])
	i++
	if n&0x80 != 0 {
		if i >= len(b) {
			return -1, -1
		}
		n = (n&0x7f)<<8 | int(b[i])
		i++
	}
	return n, i
}

func decodeUTF16String(b []byte) string {
	if len(b) < 2 {
		return ""
	}
	l := binary.LittleEndian.Uint16(b[0:])
	n := int(l & 0x7fff)
	i := 2
	if l&0x8000 != 0 { // wide form: the full length follows as a u32le
		if len(b) < 6 {
			return ""
		}
		n = int(binary.LittleEndian.Uint32(b[2:]))
		i = 6
	}
	if n < 0 {
		return ""
	}
	u := make([]uint16, 0, n)
	for j := i; j+1 < len(b) && len(u) < n; j += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[j:]))
	}
	return string(utf16.Decode(u))
}

func parseStartElement(b []byte, pool *stringPool, resIDs []uint32) (ManifestElement, error) {
	var e ManifestElement
	if len(b) < 36 {
		return e, fmt.Errorf("axml: start element too short")
	}
	hdr := int(binary.LittleEndian.Uint16(b[2:]))
	if hdr < 8 {
		hdr = 8
	}
	nameIdx := binary.LittleEndian.Uint32(b[20:])
	e.Name = pool.at(nameIdx)
	attrStart := int(binary.LittleEndian.Uint16(b[24:]))
	attrSize := int(binary.LittleEndian.Uint16(b[26:]))
	attrCount := int(binary.LittleEndian.Uint16(b[28:]))
	if attrSize < 20 {
		attrSize = 20
	}
	// The attribute records begin after the node header and the attribute
	// extension structure: headerSize + attributeStart. (attributeStart is
	// relative to the attr extension, which sits at the end of the node.)
	base := hdr + attrStart
	for i := 0; i < attrCount; i++ {
		p := base + i*attrSize
		if p+attrSize > len(b) {
			break
		}
		nsIdx := binary.LittleEndian.Uint32(b[p:])
		aName := binary.LittleEndian.Uint32(b[p+4:])
		rawIdx := binary.LittleEndian.Uint32(b[p+8:])
		dtype := b[p+15]
		ddata := binary.LittleEndian.Uint32(b[p+16:])

		a := ManifestAttr{
			NS:   pool.at(nsIdx),
			Name: pool.at(aName),
		}
		// Some APKs encode framework attribute names as resource references:
		// the high byte (!= 0xff) is a package id and the low 24 bits index the
		// resource map. Resolve those to a resource ID and, when the pool has
		// no plain name, fall the displayed name back to the pool too.
		if aName&0xff000000 != 0 && aName&0xff000000 != 0xff000000 {
			ri := int(aName & 0x00ffffff)
			if ri < len(resIDs) {
				a.ResID = resIDs[ri]
			}
		} else if int(aName) < len(resIDs) {
			a.ResID = resIDs[aName]
		}
		if rawIdx != 0xffffffff {
			a.Raw = pool.at(rawIdx)
			a.Value = a.Raw
		}
		a.Type = dtype
		switch dtype {
		case typeString:
			a.Value = pool.at(ddata)
		case typeIntDec:
			a.Int = int64(int32(ddata))
			a.Value = fmt.Sprintf("%d", a.Int)
		case typeIntHex:
			a.Int = int64(int32(ddata))
			a.Value = fmt.Sprintf("0x%x", ddata)
		case typeIntBoolean:
			a.Int = int64(ddata)
			a.Value = fmt.Sprintf("%t", ddata != 0)
		}
		e.Attrs = append(e.Attrs, a)
	}
	return e, nil
}

func applyManifestElement(m *Manifest, e ManifestElement) {
	if e.Name == "manifest" {
		for _, a := range e.Attrs {
			switch a.Name {
			case "package":
				if m.Package == "" {
					m.Package = a.Value
				}
			case "versionCode":
				m.VersionCode = a.Int
			case "versionName":
				m.VersionName = a.Value
			}
		}
	}
	if e.Name == "uses-sdk" {
		for _, a := range e.Attrs {
			switch a.Name {
			case "minSdkVersion":
				if m.MinSDK < 0 {
					m.MinSDK = a.Int
				}
			case "targetSdkVersion":
				if m.TargetSDK < 0 {
					m.TargetSDK = a.Int
				}
			}
		}
	}
	// Fall back to resource IDs when names were localised or absent.
	for _, a := range e.Attrs {
		switch a.ResID {
		case resVersionCode:
			if m.VersionCode < 0 {
				m.VersionCode = a.Int
			}
		case resVersionName:
			if m.VersionName == "" {
				m.VersionName = a.Value
			}
		case resMinSDK:
			if m.MinSDK < 0 {
				m.MinSDK = a.Int
			}
		case resTargetSDK:
			if m.TargetSDK < 0 {
				m.TargetSDK = a.Int
			}
		}
	}
}

// ParseManifestFromSet reads and parses the base APK's manifest.
func ParseManifestFromSet(s *Set) (*Manifest, error) {
	base := s.Base()
	if base == nil {
		return nil, fmt.Errorf("apk: no base APK")
	}
	raw, err := base.Read("AndroidManifest.xml")
	if err != nil {
		return nil, err
	}
	return ParseManifest(raw)
}

// String is a short human summary.
func (m *Manifest) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", m.Package)
	if m.VersionName != "" || m.VersionCode >= 0 {
		fmt.Fprintf(&b, " v%s(%d)", m.VersionName, m.VersionCode)
	}
	if m.MinSDK >= 0 || m.TargetSDK >= 0 {
		fmt.Fprintf(&b, " sdk=%d/%d", m.MinSDK, m.TargetSDK)
	}
	return b.String()
}
