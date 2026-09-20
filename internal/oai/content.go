package oai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ContentKind distinguishes the three states a message's content field can be in
// plus its absence. This is not pedantry: providers disagree about what an
// assistant message carrying tool calls should have here. Some require
// `"content": null`, some require `"content": ""`, and some reject the key
// entirely. Collapsing null and "" into one Go string makes that quirk
// unrepresentable, so the distinction is kept explicit.
type ContentKind uint8

const (
	// ContentAbsent means the key was not present in the JSON object.
	ContentAbsent ContentKind = iota
	// ContentNull means the key was present with a JSON null value.
	ContentNull
	// ContentText means the key held a JSON string.
	ContentText
	// ContentParts means the key held an array of content parts.
	ContentParts
)

// Content is a message's content field: absent, null, a string, or an array of parts.
type Content struct {
	Kind  ContentKind
	Text  string
	Parts []ContentPart
}

// TextContent builds string-valued content.
func TextContent(s string) Content { return Content{Kind: ContentText, Text: s} }

// NullContent builds explicitly-null content.
func NullContent() Content { return Content{Kind: ContentNull} }

// IsEmpty reports whether the content carries no usable text. Absent, null, and
// the empty string are all empty; a parts array is empty only if no part has text.
func (c Content) IsEmpty() bool { return c.String() == "" }

// String flattens content to text, concatenating the text of any parts. Non-text
// parts (images, audio) contribute nothing, which is correct for every caller we
// have: they want something to log, estimate tokens over, or return to a client.
func (c Content) String() string {
	switch c.Kind {
	case ContentText:
		return c.Text
	case ContentParts:
		var b strings.Builder
		for _, p := range c.Parts {
			if p.Type == "text" || p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func (c Content) MarshalJSON() ([]byte, error) {
	switch c.Kind {
	case ContentText:
		return json.Marshal(c.Text)
	case ContentParts:
		return json.Marshal(c.Parts)
	default:
		// ContentAbsent also lands here; callers that need true omission delete the
		// key after marshalling (see Message.MarshalJSON).
		return []byte("null"), nil
	}
}

func (c *Content) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "null" {
		*c = Content{Kind: ContentNull}
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = Content{Kind: ContentText, Text: s}
		return nil
	}

	var parts []ContentPart
	if err := json.Unmarshal(b, &parts); err == nil {
		*c = Content{Kind: ContentParts, Parts: parts}
		return nil
	}

	return fmt.Errorf("message content must be a string, an array of parts, or null")
}

// ContentPart is one element of a multipart content array.
type ContentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// ImageURL is an image content part's payload. URL may be a data: URI.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

var contentPartKnown = knownFields(ContentPart{})

type contentPartAlias ContentPart

func (p *ContentPart) UnmarshalJSON(b []byte) error {
	var a contentPartAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*p = ContentPart(a)

	extra, err := captureExtra(b, contentPartKnown)
	if err != nil {
		return err
	}
	p.Extra = extra
	return nil
}

func (p ContentPart) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(contentPartAlias(p))
	if err != nil {
		return nil, err
	}
	return mergeExtra(b, p.Extra)
}
