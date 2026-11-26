package sandboxalloc

import (
	"github.com/Tencent/WeKnora/internal/sandbox/allocation"
	"github.com/Tencent/WeKnora/internal/types"
)

func ToEntity(a allocation.Assignment, digest string) (*types.SessionSandboxAssignment, *types.SandboxUserMachine) {
	row := &types.SessionSandboxAssignment{
		SessionID: a.SessionID, ConfigTenantID: a.ConfigTenantID, ConfigID: a.ConfigID,
		ReleasedAt: a.ReleasedAt,
	}
	m, ok := a.UserMachine()
	if !ok {
		return row, nil
	}
	row.MachineDigest = digest
	return row, &types.SandboxUserMachine{
		MachineDigest: digest, OwnerTenantID: m.OwnerTenantID, OwnerID: m.OwnerID,
		ConfigTenantID: m.ConfigTenantID, ConfigID: m.ConfigID,
	}
}

func FromEntity(e *types.SessionSandboxAssignment, machine *types.SandboxUserMachine) allocation.Assignment {
	a := allocation.Assignment{
		SessionID: e.SessionID, ConfigTenantID: e.ConfigTenantID, ConfigID: e.ConfigID,
		ReleasedAt: e.ReleasedAt,
	}
	if e.MachineDigest != "" && machine != nil {
		a.Machine = allocation.UserMachine{
			OwnerTenantID: machine.OwnerTenantID, OwnerID: machine.OwnerID,
			ConfigTenantID: machine.ConfigTenantID, ConfigID: machine.ConfigID,
		}
	}
	return a
}
