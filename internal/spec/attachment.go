package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Attachment is an image given to an agent with its input: with a steer
// (POST /v1/runs/{id}/input) or with the workload's first prompt
// (workload.attachments).
type Attachment struct {
	Name        string `json:"name" yaml:"name" doc:"Shown to people; 1-255 bytes of UTF-8 without /, \\, NUL or control characters. Never used as a path as given."`
	ContentType string `json:"contentType" yaml:"contentType" doc:"image/png, image/jpeg, image/webp or image/gif; the bytes' magic number must match."`
	Data        string `json:"data,omitempty" yaml:"data,omitempty" doc:"Required: the image, standard base64 (no data: prefix), at most 5 MiB decoded. Run views leave it out."`
	// Path is where the shim wrote the image in the container; the shim
	// sets it, nobody else.
	Path string `json:"-" yaml:"-"`
}

// AttachmentMeta is what records and events say of an attachment: never
// its bytes.
type AttachmentMeta struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	SHA256      string `json:"sha256"`
}

// Attachment limits (docs/runspec.md).
const (
	MaxAttachments     = 10
	MaxAttachmentBytes = 5 << 20
	maxAttachmentName  = 255
)

// attachmentMagic: the content types lux takes, each with a check of the
// decoded bytes' magic number.
var attachmentMagic = map[string]func([]byte) bool{
	"image/png":  func(b []byte) bool { return bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) },
	"image/jpeg": func(b []byte) bool { return bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}) },
	"image/gif": func(b []byte) bool {
		return bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a"))
	},
	"image/webp": func(b []byte) bool {
		return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP"
	},
}

// SniffImage is the content type of an image lux takes, from its magic
// number; "" if it is none of them.
func SniffImage(b []byte) string {
	for _, t := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		if attachmentMagic[t](b) {
			return t
		}
	}
	return ""
}

// CheckAttachments lists what is wrong with a list of attachments; at
// names the list in the messages (e.g. "attachments").
func CheckAttachments(at string, list []Attachment) []string {
	var errs []string
	if len(list) > MaxAttachments {
		errs = append(errs, fmt.Sprintf("%s: %d attachments, at most %d", at, len(list), MaxAttachments))
	}
	for i, a := range list {
		if err := checkAttachment(a); err != "" {
			errs = append(errs, fmt.Sprintf("%s[%d]: %s", at, i, err))
		}
	}
	return errs
}

func checkAttachment(a Attachment) string {
	if msg := checkAttachmentName(a.Name); msg != "" {
		return msg
	}
	magic, ok := attachmentMagic[a.ContentType]
	if !ok {
		return fmt.Sprintf("contentType %q: need image/png, image/jpeg, image/webp or image/gif", a.ContentType)
	}
	// Refused before decoding: base64 is 4 characters per 3 bytes.
	if a.Data == "" {
		return "data is required"
	}
	if base64.StdEncoding.DecodedLen(len(a.Data)) > MaxAttachmentBytes+2 {
		return fmt.Sprintf("too big: more than %d bytes decoded", MaxAttachmentBytes)
	}
	b, err := base64.StdEncoding.Strict().DecodeString(a.Data)
	if err != nil {
		return "data is not standard base64: " + err.Error()
	}
	if len(b) > MaxAttachmentBytes {
		return fmt.Sprintf("too big: %d bytes decoded, at most %d", len(b), MaxAttachmentBytes)
	}
	if !magic(b) {
		got := SniffImage(b)
		if got == "" {
			got = "not an image lux takes"
		}
		return fmt.Sprintf("contentType %s does not match its bytes (%s)", a.ContentType, got)
	}
	return ""
}

func checkAttachmentName(name string) string {
	switch {
	case name == "":
		return "name is required"
	case len(name) > maxAttachmentName:
		return fmt.Sprintf("name is %d bytes, at most %d", len(name), maxAttachmentName)
	case !utf8.ValidString(name):
		return "name is not valid UTF-8"
	case strings.ContainsAny(name, `/\`):
		return `name must not contain / or \`
	case strings.ContainsFunc(name, unicode.IsControl):
		return "name must not contain NUL or control characters"
	}
	return ""
}

// Decoded is the attachment's bytes; nil if its data is not base64 (a
// checked attachment always is).
func (a Attachment) Decoded() []byte {
	b, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		return nil
	}
	return b
}

// Meta is what records say of the attachment.
func (a Attachment) Meta() AttachmentMeta {
	b := a.Decoded()
	sum := sha256.Sum256(b)
	return AttachmentMeta{Name: a.Name, ContentType: a.ContentType, Size: len(b), SHA256: hex.EncodeToString(sum[:])}
}

// AttachmentsMeta is Meta of each; nil for none.
func AttachmentsMeta(list []Attachment) []AttachmentMeta {
	if len(list) == 0 {
		return nil
	}
	out := make([]AttachmentMeta, len(list))
	for i, a := range list {
		out[i] = a.Meta()
	}
	return out
}
