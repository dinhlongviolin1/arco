package daemon

// rev7/T3.3: a typo'd VM fleet must not half-start — every bad [[vms]]
// definition (option-injection host, empty/duplicate name, a default_vm no
// entry backs) refuses daemon startup before any listener exists. Uses an
// injected Fake VM so use_local_vm doesn't require a herdr binary here; the
// fleet registry is built regardless.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/config"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

func fleetCfg(t *testing.T, vms []config.VMDef, defaultVM string) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DBPath = filepath.Join(dir, "arco.db")
	cfg.Socket = filepath.Join(dir, "arco.sock")
	cfg.UseLocalVM = true
	cfg.VMs = vms
	cfg.DefaultVM = defaultVM
	return cfg
}

func TestRun_FleetBadDefsFailStartup(t *testing.T) {
	cases := []struct {
		name string
		vms  []config.VMDef
		dvm  string
		want string
	}{
		{"option-injection host", []config.VMDef{{Name: "vm1", Host: "-oProxyCommand=x"}}, "", "option injection"},
		{"empty host", []config.VMDef{{Name: "vm1", Host: ""}}, "", "empty ssh host"},
		{"empty name", []config.VMDef{{Name: "", Host: "vm1.internal"}}, "", "empty name"},
		{"duplicate name", []config.VMDef{{Name: "vm1", Host: "a.internal"}, {Name: "vm1", Host: "b.internal"}}, "", "duplicate"},
		{"default_vm without entry", []config.VMDef{{Name: "vm1", Host: "a.internal"}}, "vm9", "no [[vms]] entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(context.Background(), fleetCfg(t, tc.vms, tc.dvm), Deps{VM: vm.NewFake()})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
