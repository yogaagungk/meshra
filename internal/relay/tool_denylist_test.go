package relay

import (
	"errors"
	"strings"
	"testing"

	"meshra/internal/model"
)

func TestDenyToolAction(t *testing.T) {
	tests := []struct {
		name   string
		args   any
		denied bool
	}{
		{"root rm argv", &toolCommandArgs{Argv: []string{"/bin/rm", "-rf", "/"}}, true},
		{"root rm split flags", &toolCommandArgs{Argv: []string{"rm", "-r", "-f", "/./"}}, true},
		{"root rm long option", &toolCommandArgs{Shell: `sudo rm --recursive --force /`}, true},
		{"quoted root glob", &toolCommandArgs{Shell: `rm -Rf "/"*`}, true},
		{"nested shell text", &toolCommandArgs{Shell: `sh -c 'rm -rf /'`}, true},
		{"fork bomb", &toolCommandArgs{Shell: `:(){ :|:& };:`}, true},
		{"named fork bomb", &toolCommandArgs{Shell: `boom() { boom | boom & }; boom`}, true},
		{"disk dd", &toolCommandArgs{Argv: []string{"dd", "if=image", "of=/dev/nvme0n1"}}, true},
		{"disk redirect", &toolCommandArgs{Shell: `printf x > /dev/sda`}, true},
		{"disk symlink directory", &toolCommandArgs{Shell: `tee /dev/disk/by-id/my-disk`}, true},
		{"disk direct write", &toolWriteArgs{Path: "/dev/sda"}, true},
		{"ssh write", &toolWriteArgs{Path: ".ssh/authorized_keys"}, true},
		{"aws normalized write", &toolWriteArgs{Path: "dir/../.aws/credentials"}, true},
		{"cloud shell write", &toolCommandArgs{Shell: `echo x > "${HOME}/.config/gcloud/credentials.db"`}, true},
		{"meshra key write", &toolWriteArgs{Path: "automation.key"}, true},
		{"meshra config write", &toolCommandArgs{Argv: []string{"cp", "source", "/home/a/.config/meshra/relay.db"}}, true},
		{"embedded secret write", &toolCommandArgs{Argv: []string{"python", "-c", `open('/home/a/.ssh/id_rsa','w').write('x')`}}, true},
		{"ordinary removal", &toolCommandArgs{Argv: []string{"rm", "-rf", "build"}}, false},
		{"nonrecursive root", &toolCommandArgs{Argv: []string{"rm", "/"}}, false},
		{"ordinary write", &toolWriteArgs{Path: "src/main.go", Content: "hello"}, false},
		{"similar path", &toolWriteArgs{Path: ".ssh-backup/config"}, false},
		{"read secret path", &toolCommandArgs{Argv: []string{"cat", ".ssh/config"}}, false},
		{"read redirected to secret", &toolCommandArgs{Shell: `cat input > .ssh/config`}, true},
		{"unrelated action", &toolReadArgs{Path: ".ssh/config"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := denyToolAction(tc.args)
			if !tc.denied {
				if err != nil {
					t.Fatalf("unexpected denial: %v", err)
				}
				return
			}
			var apiErr *model.Error
			if !errors.As(err, &apiErr) || apiErr.Status != 403 || !strings.Contains(apiErr.Message, "hardline deny") {
				t.Fatalf("expected clear 403 denial, got %v", err)
			}
		})
	}
}
