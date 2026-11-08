// Package claim decodes LBRY claim values (the data in OP_CLAIM_NAME / OP_UPDATE_CLAIM
// outputs) into the fields the search index needs. It reads the current format (v2:
// unsigned 0x00 or signed 0x01 + channel + signature, then protobuf), the legacy v1
// protobuf (2018–19) and the original JSON claims.
package claim

import (
	"encoding/hex"
	"encoding/json"
	"strings"
)

type Type string

const (
	Stream     Type = "stream"
	Channel    Type = "channel"
	Repost     Type = "repost"
	Collection Type = "collection"
	Unknown    Type = ""
)

// Meta is what the index keeps about a claim.
type Meta struct {
	Type        Type
	Version     int    // 0 = old JSON, 1 = legacy protobuf, 2 = current
	ChannelID   string // signing channel, display hex ("" if unsigned)
	Title       string
	Description string
	Author      string
	Tags        []string
	Languages   []string
	Thumbnail   string
	MediaType   string
	Duration    uint32 // seconds (video or audio)
	ReleaseTime int64  // unix seconds, 0 if not set
	RepostOf    string // reposted claim ID, display hex
	FeeAmount   uint64 // dewies for LBC, cents for USD
	FeeCurrency string
	SDHash      string
}

// Decode never fails outright: anything unreadable comes back as Unknown with no fields.
func Decode(value []byte) Meta {
	if len(value) == 0 {
		return Meta{}
	}
	switch value[0] {
	case 0x00:
		if m, ok := decodeV2(value[1:]); ok {
			return m
		}
	case 0x01:
		if len(value) >= 85 {
			if m, ok := decodeV2(value[85:]); ok {
				m.ChannelID = hexReversed(value[1:21])
				return m
			}
		}
	case '{':
		return decodeJSON(value)
	}
	return decodeV1(value)
}

func hexReversed(b []byte) string {
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[len(b)-1-i]
	}
	return hex.EncodeToString(out)
}

func decodeV2(b []byte) (Meta, bool) {
	c := parse(b)
	if c == nil {
		return Meta{}, false
	}
	m := Meta{
		Version:     2,
		Title:       c.str(8),
		Description: c.str(9),
		Thumbnail:   c.sub(10).str(5),
	}
	for _, t := range c.repeatedBytes(11) {
		m.Tags = append(m.Tags, string(t))
	}
	for _, l := range c.repeatedBytes(12) {
		if code := languageCode(parse(l).varint(1)); code != "" {
			m.Languages = append(m.Languages, code)
		}
	}
	switch {
	case c.has(1):
		m.Type = Stream
		s := c.sub(1)
		src := s.sub(1)
		m.MediaType = src.str(4)
		m.SDHash = hex.EncodeToString(src.bytes(6))
		m.Author = s.str(2)
		m.ReleaseTime = int64(s.varint(5))
		if fee := s.sub(6); fee != nil {
			m.FeeAmount = fee.varint(3)
			m.FeeCurrency = [...]string{"", "LBC", "BTC", "USD"}[min(int(fee.varint(1)), 3)]
		}
		if v := s.sub(11); v != nil {
			m.Duration = uint32(v.varint(3))
		} else if a := s.sub(12); a != nil {
			m.Duration = uint32(a.varint(1))
		}
	case c.has(2):
		m.Type = Channel
	case c.has(3):
		m.Type = Collection
	case c.has(4):
		m.Type = Repost
		m.RepostOf = hexReversed(c.sub(4).bytes(1))
	default:
		return Meta{}, false
	}
	return m, true
}

// Legacy v1 protobuf: Claim{claimType 2, stream 3, certificate 4, publisherSignature 5}.
func decodeV1(b []byte) Meta {
	c := parse(b)
	if c == nil {
		return Meta{}
	}
	m := Meta{Version: 1}
	if c.varint(2) == 2 || c.has(4) {
		m.Type = Channel
		return m
	}
	s := c.sub(3)
	if s == nil {
		return Meta{}
	}
	m.Type = Stream
	md := s.sub(2)
	m.Title = md.str(3)
	m.Description = md.str(4)
	m.Author = md.str(5)
	m.Thumbnail = md.str(9)
	if code := languageCode(md.varint(2)); code != "" {
		m.Languages = []string{code}
	}
	if md.varint(7) != 0 {
		m.Tags = []string{"mature"}
	}
	if fee := md.sub(8); fee != nil {
		m.FeeCurrency = [...]string{"", "LBC", "BTC", "USD"}[min(int(fee.varint(1)), 3)]
	}
	src := s.sub(3)
	m.MediaType = src.str(4)
	m.SDHash = hex.EncodeToString(src.bytes(3))
	if sig := c.sub(5); sig != nil {
		if id := sig.bytes(4); len(id) == 20 {
			m.ChannelID = hex.EncodeToString(id) // v1 stored the certificate ID in display order
		}
	}
	return m
}

// The original JSON claims (2016–17).
func decodeJSON(b []byte) Meta {
	var j struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Author      string `json:"author"`
		Thumbnail   string `json:"thumbnail"`
		ContentType string `json:"content-type"`
		NSFW        bool   `json:"nsfw"`
		Language    string `json:"language"`
	}
	if json.Unmarshal(b, &j) != nil {
		return Meta{}
	}
	m := Meta{Type: Stream, Version: 0, Title: j.Title, Description: j.Description, Author: j.Author,
		Thumbnail: j.Thumbnail, MediaType: j.ContentType}
	if j.NSFW {
		m.Tags = []string{"mature"}
	}
	if l := strings.ToLower(j.Language); l == "en" || l == "english" {
		m.Languages = []string{"en"}
	}
	return m
}

func languageCode(n uint64) string {
	if n < uint64(len(languageCodes)) {
		return languageCodes[n]
	}
	return ""
}
