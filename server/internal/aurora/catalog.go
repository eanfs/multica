// Package aurora holds the Aurora content-creation domain: the static skill
// catalog, and the credit accounting that pays for generations.
package aurora

import (
	"slices"
	"strings"
)

// SkillCatalogEntry is one consumer-facing skill in the directory.
type SkillCatalogEntry struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`     // Chinese display name
	NameEn    string   `json:"name_en"`  // English display name
	Category  string   `json:"category"` // image | video | content | office
	Credits   int      `json:"credits"`
	Input     []string `json:"input"`
	Output    []string `json:"output"`
	Featured  bool     `json:"featured"`
	Available bool     `json:"available"` // false for phase-2 skills
	// AttachmentRules is the input policy projected onto the wire. It is always
	// an array (empty for skills that take no attachments) so the client never
	// has to distinguish "none" from "an older server".
	AttachmentRules []AttachmentConstraint `json:"attachment_rules"`
}

var catalog = []SkillCatalogEntry{
	{ID: "poster", Name: "海报制作", NameEn: "Poster", Category: "image", Credits: 760, Input: []string{"text", "image"}, Output: []string{"image"}, Featured: true, Available: true},
	{ID: "xhs-image", Name: "小红书图片", NameEn: "Xiaohongshu Image", Category: "image", Credits: 620, Input: []string{"text", "image"}, Output: []string{"image"}, Featured: true, Available: true},
	{ID: "product-image", Name: "商品图制作", NameEn: "Product Image", Category: "image", Credits: 860, Input: []string{"text", "image"}, Output: []string{"image"}, Available: true},
	{ID: "text-image", Name: "文字生成图片", NameEn: "Text to Image", Category: "image", Credits: 680, Input: []string{"text"}, Output: []string{"image"}, Available: true},
	{ID: "image-edit", Name: "图片修改", NameEn: "Image Edit", Category: "image", Credits: 520, Input: []string{"text", "image"}, Output: []string{"image"}, Available: true},
	{ID: "id-photo", Name: "证件照制作", NameEn: "ID Photo", Category: "image", Credits: 360, Input: []string{"image", "text"}, Output: []string{"image"}, Available: true},
	{ID: "image-video", Name: "图片生成视频", NameEn: "Image to Video", Category: "video", Credits: 1880, Input: []string{"image", "text"}, Output: []string{"video"}, Available: true},
	{ID: "text-video", Name: "文字生成视频", NameEn: "Text to Video", Category: "video", Credits: 1680, Input: []string{"text"}, Output: []string{"video"}, Available: true},
	{ID: "video-captions", Name: "视频剪辑与字幕", NameEn: "Video Captions", Category: "video", Credits: 980, Input: []string{"video", "text"}, Output: []string{"video"}, Available: true},
	{ID: "avatar-video", Name: "数字人口播", NameEn: "Avatar Video", Category: "video", Credits: 1480, Input: []string{"text", "image", "audio"}, Output: []string{"video"}, Available: false},
	{ID: "xhs-copy", Name: "小红书文案", NameEn: "Xiaohongshu Copy", Category: "content", Credits: 260, Input: []string{"text", "document"}, Output: []string{"text"}, Available: true},
	{ID: "resume", Name: "简历制作", NameEn: "Resume", Category: "office", Credits: 420, Input: []string{"text", "document"}, Output: []string{"pdf", "text"}, Available: true},
	{ID: "document-summary", Name: "文件总结", NameEn: "Document Summary", Category: "office", Credits: 380, Input: []string{"document", "text"}, Output: []string{"text"}, Available: true},
	{ID: "transcription", Name: "录音转文字", NameEn: "Transcription", Category: "office", Credits: 300, Input: []string{"audio", "video"}, Output: []string{"text"}, Available: true},
	{ID: "ppt", Name: "PPT 制作", NameEn: "PPT", Category: "office", Credits: 820, Input: []string{"text", "document", "spreadsheet", "image"}, Output: []string{"pptx", "pdf"}, Available: false},
	{ID: "excel", Name: "Excel 数据分析", NameEn: "Excel Analysis", Category: "office", Credits: 460, Input: []string{"spreadsheet", "text"}, Output: []string{"xlsx", "pdf", "text"}, Available: false},
}

// Catalog returns an independent copy of the 16 skills in fixed display order,
// each carrying its execution policy's attachment rules. avatar-video, ppt and
// excel are listed but unavailable until phase 2.
func Catalog() []SkillCatalogEntry {
	entries := make([]SkillCatalogEntry, len(catalog))
	for i, entry := range catalog {
		entries[i] = attachAttachmentRules(cloneEntry(entry))
	}
	return entries
}

// Lookup returns an independent copy of the entry for id, or false when unknown.
func Lookup(id string) (SkillCatalogEntry, bool) {
	entry, ok := lookupCatalogEntry(id)
	if !ok {
		return SkillCatalogEntry{}, false
	}
	return attachAttachmentRules(entry), true
}

// lookupCatalogEntry is Lookup without the policy projection. ExecutionPolicy
// uses it so the catalog and the policy cannot recurse into each other.
func lookupCatalogEntry(id string) (SkillCatalogEntry, bool) {
	for _, entry := range catalog {
		if entry.ID == id {
			return cloneEntry(entry), true
		}
	}
	return SkillCatalogEntry{}, false
}

// attachAttachmentRules projects the one policy table onto an entry. The result
// is always a non-nil slice: the wire contract is an array, never null.
func attachAttachmentRules(entry SkillCatalogEntry) SkillCatalogEntry {
	entry.AttachmentRules = []AttachmentConstraint{}
	if policy, ok := executionPolicies[entry.ID]; ok && entry.Available {
		if rules := cloneConstraints(policy.Attachments); len(rules) > 0 {
			entry.AttachmentRules = rules
		}
	}
	return entry
}

// Exists reports whether id names a catalog entry, including unavailable skills.
func Exists(id string) bool {
	for i := range catalog {
		if catalog[i].ID == id {
			return true
		}
	}
	return false
}

// AssetKind maps a reported artifact onto the aurora_asset.kind category. The
// daemon reports a file format; the catalog's Output names the categories the
// skill may produce. An exact format match among the outputs wins — it
// disambiguates multi-output skills (resume → pdf|text) — otherwise the format
// is mapped to its category and accepted only when the skill lists it. An
// unlisted format is rejected rather than silently recorded as the primary
// output. An empty format still falls back to the primary output.
func AssetKind(entry SkillCatalogEntry, format string) (string, bool) {
	if format != "" {
		for _, out := range entry.Output {
			if strings.EqualFold(out, format) {
				return out, true
			}
		}
		category, ok := assetCategoryForFormat(format)
		if !ok || !slices.Contains(entry.Output, category) {
			return "", false
		}
		return category, true
	}
	if len(entry.Output) > 0 {
		return entry.Output[0], true
	}
	return entry.Category, true
}

// assetCategoryForFormat maps a concrete file format onto the catalog output
// category it belongs to.
func assetCategoryForFormat(format string) (string, bool) {
	switch strings.ToLower(format) {
	case "png", "jpg", "jpeg", "webp", "gif":
		return "image", true
	case "mp4", "mov", "webm":
		return "video", true
	case "md", "markdown", "txt", "text":
		return "text", true
	case "pdf":
		return "pdf", true
	case "wav", "mp3", "ogg", "opus":
		return "audio", true
	case "docx", "doc":
		return "document", true
	}
	return "", false
}

func cloneEntry(entry SkillCatalogEntry) SkillCatalogEntry {
	entry.Input = slices.Clone(entry.Input)
	entry.Output = slices.Clone(entry.Output)
	entry.AttachmentRules = cloneConstraints(entry.AttachmentRules)
	return entry
}
