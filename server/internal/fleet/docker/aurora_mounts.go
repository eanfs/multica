package docker

// Aurora node tmpfs surfaces. Every writable directory is nosuid, nodev and
// noexec; executables live in the read-only image.
const (
	auroraWorkspaceTmpfs = "rw,nosuid,nodev,noexec,size=2147483648,uid=10001,gid=10001,mode=0700"
	auroraTmpTmpfs       = "rw,nosuid,nodev,noexec,size=268435456,uid=10001,gid=10001,mode=0700"
	auroraRunTmpfs       = "rw,nosuid,nodev,noexec,size=16777216,uid=10001,gid=10001,mode=0755"
)
