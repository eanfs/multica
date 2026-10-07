package docker

import (
	"strings"
	"testing"
)

// Docker resolves a container's user spec through a chrooted getent when it
// copies an archive into that container, and Docker 25 fails for the "uid:gid"
// form (observed on the Amazon Linux 2023 host running Docker 25.0.16:
// getent unable to find entry "10001:10001" in passwd database). Every helper
// container receives the bootstrap archive, so it must keep a bare numeric uid
// that resolves and still runs as 10001:10001 through the image's passwd.
func TestHelperUserIsGetentSafe(t *testing.T) {
	if helperUser == "" || strings.Contains(helperUser, ":") {
		t.Fatalf("helper user %q must be a bare uid: the uid:gid form breaks archive copies on Docker 25", helperUser)
	}
}
