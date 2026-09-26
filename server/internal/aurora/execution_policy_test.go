package aurora_test

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/aurora"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// Fixed fixture identities for the attachment scope checks. They never touch a
// database: ValidateSkillAttachmentScope is a pure comparison over the loaded
// rows.
const (
	attachmentTestWorkspace = "11111111-1111-1111-1111-111111111111"
	attachmentTestUser      = "22222222-2222-2222-2222-222222222222"
	attachmentTestOtherUser = "33333333-3333-3333-3333-333333333333"
)

func pgUUID(t *testing.T, raw string) pgtype.UUID {
	t.Helper()
	parsed, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", raw, err)
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}
}

// kindFixture is the filename + sniffed MIME a valid upload of each kind
// carries. The validator must accept exactly these pairings.
var kindFixture = map[string]struct {
	filename    string
	contentType string
}{
	"image":    {"photo.png", "image/png"},
	"document": {"notes.md", "text/markdown"},
	"audio":    {"voice.mp3", "audio/mpeg"},
	"video":    {"clip.mp4", "video/mp4"},
}

func attachmentRow(id, filename, contentType string, size int64) db.Attachment {
	parsed := uuid.MustParse(id)
	workspace := uuid.MustParse(attachmentTestWorkspace)
	user := uuid.MustParse(attachmentTestUser)
	return db.Attachment{
		ID:           pgtype.UUID{Bytes: parsed, Valid: true},
		WorkspaceID:  pgtype.UUID{Bytes: workspace, Valid: true},
		UploaderType: "member",
		UploaderID:   pgtype.UUID{Bytes: user, Valid: true},
		Filename:     filename,
		Url:          "https://storage.test/" + id + filepath.Ext(filename),
		ContentType:  contentType,
		SizeBytes:    size,
	}
}

func attachmentOfKind(id, kind string) db.Attachment {
	fixture, ok := kindFixture[kind]
	if !ok {
		panic("unknown attachment kind " + kind)
	}
	return attachmentRow(id, fixture.filename, fixture.contentType, 1<<20)
}

func attachmentID(i int) string {
	return fmt.Sprintf("aaaaaaaa-0000-0000-0000-%012d", i)
}

// TestValidateSkillInputsMatrix is the acceptance matrix for the input rules in
// the plan: every available skill's declared attachment set, plus the shape
// failures (too many files, mixed kinds, none where one is required).
func TestValidateSkillInputsMatrix(t *testing.T) {
	cases := []struct {
		name  string
		skill string
		kinds []string
		want  bool
	}{
		{"poster accepts no files", "poster", nil, true},
		{"poster accepts four images", "poster", []string{"image", "image", "image", "image"}, true},
		{"poster rejects five images", "poster", []string{"image", "image", "image", "image", "image"}, false},
		{"poster rejects a mixed batch", "poster", []string{"image", "document"}, false},
		{"xhs-image accepts no files", "xhs-image", nil, true},
		{"product-image accepts reference images", "product-image", []string{"image", "image"}, true},
		{"text-image accepts no files", "text-image", nil, true},
		{"text-image rejects an image", "text-image", []string{"image"}, false},
		{"image-edit accepts one image", "image-edit", []string{"image"}, true},
		{"image-edit accepts four images", "image-edit", []string{"image", "image", "image", "image"}, true},
		{"image-edit rejects no files", "image-edit", nil, false},
		{"image-edit rejects five images", "image-edit", []string{"image", "image", "image", "image", "image"}, false},
		{"image-edit rejects a mixed batch", "image-edit", []string{"image", "document"}, false},
		{"id-photo accepts exactly one image", "id-photo", []string{"image"}, true},
		{"id-photo rejects two images", "id-photo", []string{"image", "image"}, false},
		{"image-video accepts exactly one image", "image-video", []string{"image"}, true},
		{"image-video rejects two images", "image-video", []string{"image", "image"}, false},
		{"text-video accepts no files", "text-video", nil, true},
		{"text-video rejects an image", "text-video", []string{"image"}, false},
		{"video-captions accepts one video", "video-captions", []string{"video"}, true},
		{"video-captions rejects an image", "video-captions", []string{"image"}, false},
		{"xhs-copy accepts no files", "xhs-copy", nil, true},
		{"xhs-copy accepts one document", "xhs-copy", []string{"document"}, true},
		{"xhs-copy rejects two documents", "xhs-copy", []string{"document", "document"}, false},
		{"resume accepts no files", "resume", nil, true},
		{"resume accepts one document", "resume", []string{"document"}, true},
		{"document-summary accepts one document", "document-summary", []string{"document"}, true},
		{"document-summary rejects no files", "document-summary", nil, false},
		{"transcription accepts audio", "transcription", []string{"audio"}, true},
		{"transcription accepts video", "transcription", []string{"video"}, true},
		{"transcription rejects audio and video together", "transcription", []string{"audio", "video"}, false},
		{"transcription rejects a document", "transcription", []string{"document"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, ok := aurora.ExecutionPolicy(tc.skill)
			if !ok {
				t.Fatalf("ExecutionPolicy(%q) not found", tc.skill)
			}
			files := make([]db.Attachment, 0, len(tc.kinds))
			for i, kind := range tc.kinds {
				files = append(files, attachmentOfKind(attachmentID(i), kind))
			}

			err := aurora.ValidateSkillInputs(policy, files)
			if tc.want && err != nil {
				t.Fatalf("ValidateSkillInputs(%s) = %v, want nil", tc.skill, err)
			}
			if !tc.want && err == nil {
				t.Fatalf("ValidateSkillInputs(%s) = nil, want an error", tc.skill)
			}
		})
	}

	// The remaining rejection classes live here too so the named matrix is
	// self-contained; the sibling tests below exercise their variations.
	t.Run("unavailable skills have no policy", func(t *testing.T) {
		if _, ok := aurora.ExecutionPolicy("avatar-video"); ok {
			t.Fatal("avatar-video has a policy")
		}
	})

	t.Run("MIME and extension mismatch is rejected", func(t *testing.T) {
		policy, _ := aurora.ExecutionPolicy("poster")
		file := attachmentRow(attachmentID(0), "photo.png", "application/pdf", 1024)
		if err := aurora.ValidateSkillInputs(policy, []db.Attachment{file}); err == nil {
			t.Fatal("mismatched MIME accepted")
		}
	})

	t.Run("missing storage object is rejected", func(t *testing.T) {
		policy, _ := aurora.ExecutionPolicy("poster")
		file := attachmentOfKind(attachmentID(0), "image")
		file.Url = ""
		if err := aurora.ValidateSkillInputs(policy, []db.Attachment{file}); err == nil {
			t.Fatal("row without a storage object accepted")
		}
	})

	t.Run("foreign workspace and owner are rejected", func(t *testing.T) {
		file := attachmentOfKind(attachmentID(0), "image")
		workspace := pgUUID(t, attachmentTestWorkspace)
		user := pgUUID(t, attachmentTestUser)
		if err := aurora.ValidateSkillAttachmentScope([]db.Attachment{file}, pgUUID(t, "44444444-4444-4444-4444-444444444444"), user); err == nil {
			t.Fatal("foreign workspace accepted")
		}
		if err := aurora.ValidateSkillAttachmentScope([]db.Attachment{file}, workspace, pgUUID(t, "55555555-5555-5555-5555-555555555555")); err == nil {
			t.Fatal("foreign owner accepted")
		}
	})
}

// TestValidateSkillInputsRejectsBadFiles covers the per-file failures the
// sniffed metadata must catch before provisioning.
func TestValidateSkillInputsRejectsBadFiles(t *testing.T) {
	const mib = 1 << 20
	poster, _ := aurora.ExecutionPolicy("poster")
	documents, _ := aurora.ExecutionPolicy("document-summary")
	transcription, _ := aurora.ExecutionPolicy("transcription")

	missingObject := attachmentRow(attachmentID(0), "photo.png", "image/png", 1024)
	missingObject.Url = ""
	anonymous := attachmentRow(attachmentID(0), "photo.png", "image/png", 1024)
	anonymous.ID = pgtype.UUID{}

	cases := []struct {
		name   string
		policy aurora.SkillExecutionPolicy
		file   db.Attachment
	}{
		{"extension and sniffed MIME disagree", poster, attachmentRow(attachmentID(0), "photo.png", "application/pdf", 1024)},
		{"unknown extension", poster, attachmentRow(attachmentID(0), "photo.gif", "image/gif", 1024)},
		{"MIME and extension swap image formats", poster, attachmentRow(attachmentID(0), "photo.png", "image/jpeg", 1024)},
		{"missing storage object", poster, missingObject},
		{"missing row id", poster, anonymous},
		{"zero size", poster, attachmentRow(attachmentID(0), "photo.png", "image/png", 0)},
		{"image over the 25 MiB cap", poster, attachmentRow(attachmentID(0), "photo.png", "image/png", 25*mib+1)},
		{"document over the 25 MiB cap", documents, attachmentRow(attachmentID(1), "notes.pdf", "application/pdf", 25*mib+1)},
		{"audio over the 100 MiB cap", transcription, attachmentRow(attachmentID(2), "voice.mp3", "audio/mpeg", 100*mib+1)},
		{"document format on an audio-only skill", transcription, attachmentRow(attachmentID(3), "notes.md", "text/markdown", 1024)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := aurora.ValidateSkillInputs(tc.policy, []db.Attachment{tc.file}); err == nil {
				t.Fatalf("ValidateSkillInputs(%s) = nil, want an error", tc.name)
			}
		})
	}

	// The boundary itself is accepted: exactly 25 MiB is inside the image cap.
	if err := aurora.ValidateSkillInputs(poster, []db.Attachment{
		attachmentRow(attachmentID(0), "photo.png", "image/png", 25*mib),
	}); err != nil {
		t.Fatalf("25 MiB image rejected: %v", err)
	}
}

// TestValidateSkillInputsRejectsForeignWorkspaceOrOwner pins the scope half of
// the input contract: an attachment row only counts when it belongs to the
// requesting workspace and the requesting human uploader.
func TestValidateSkillInputsRejectsForeignWorkspaceOrOwner(t *testing.T) {
	workspace := pgUUID(t, attachmentTestWorkspace)
	user := pgUUID(t, attachmentTestUser)
	file := attachmentOfKind(attachmentID(0), "image")

	if err := aurora.ValidateSkillAttachmentScope([]db.Attachment{file}, workspace, user); err != nil {
		t.Fatalf("same-workspace, same-owner row rejected: %v", err)
	}

	otherWorkspace := pgUUID(t, "44444444-4444-4444-4444-444444444444")
	if err := aurora.ValidateSkillAttachmentScope([]db.Attachment{file}, otherWorkspace, user); err == nil {
		t.Fatal("row from a foreign workspace accepted")
	}

	otherUser := pgUUID(t, "55555555-5555-5555-5555-555555555555")
	if err := aurora.ValidateSkillAttachmentScope([]db.Attachment{file}, workspace, otherUser); err == nil {
		t.Fatal("row uploaded by another user accepted")
	}

	agentUpload := file
	agentUpload.UploaderType = "agent"
	if err := aurora.ValidateSkillAttachmentScope([]db.Attachment{agentUpload}, workspace, user); err == nil {
		t.Fatal("agent-uploaded row accepted as a human input")
	}
}

// TestExecutionPolicyCoversEveryAvailableSkill asserts the policy table is
// complete for the 13 runnable skills and absent for the phase-2 ones.
func TestExecutionPolicyCoversEveryAvailableSkill(t *testing.T) {
	available := 0
	for _, entry := range aurora.Catalog() {
		policy, ok := aurora.ExecutionPolicy(entry.ID)
		if !entry.Available {
			if ok {
				t.Errorf("unavailable skill %q has an execution policy", entry.ID)
			}
			continue
		}
		available++
		if !ok {
			t.Errorf("available skill %q has no execution policy", entry.ID)
			continue
		}
		if policy.SkillID != entry.ID {
			t.Errorf("policy SkillID = %q, want %q", policy.SkillID, entry.ID)
		}
		if policy.Route == "" {
			t.Errorf("skill %q has an empty route", entry.ID)
		}
		if !reflect.DeepEqual(policy.OutputKinds, entry.Output) {
			t.Errorf("skill %q output kinds = %#v, want %#v", entry.ID, policy.OutputKinds, entry.Output)
		}
	}
	if available != 13 {
		t.Fatalf("available skills with policies = %d, want 13", available)
	}

	for _, id := range []string{"", "nope", "avatar-video", "ppt", "excel"} {
		if _, ok := aurora.ExecutionPolicy(id); ok {
			t.Errorf("ExecutionPolicy(%q) = ok, want absent", id)
		}
	}
}
