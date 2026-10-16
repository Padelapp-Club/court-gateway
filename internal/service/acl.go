package service

// Well-known SIDs of the Windows accounts that may read the secrets: the
// local Administrators group and LocalSystem, which runs the service.
// SIDs, not names: the names are localized.
const (
	SIDAdministrators = "S-1-5-32-544"
	SIDSystem         = "S-1-5-18"
)

// ACE is one entry of a Windows DACL as read back: its type and SID.
type ACE struct {
	Type uint8 // 0 = ACCESS_ALLOWED_ACE_TYPE
	SID  string
}

// ACLProblem judges the DACL of the folder holding .env, of .env itself or
// of the hub's signing key file: it must not inherit (protected), must
// exist (a null DACL allows everyone) and must hold allow entries for
// exactly Administrators and SYSTEM — no other entry of any type. ""
// means it is closed.
func ACLProblem(protected, nullDACL bool, aces []ACE) string {
	switch {
	case nullDACL:
		return "нет списка доступа: читает любой пользователь"
	case !protected:
		return "права наследуются от родительской папки: секреты могут прочитать другие пользователи"
	}
	seen := map[string]bool{}
	for _, a := range aces {
		if a.Type != 0 || (a.SID != SIDAdministrators && a.SID != SIDSystem) {
			return "в списке доступа есть лишняя запись (" + a.SID + "): доступ не только у администраторов и системы"
		}
		seen[a.SID] = true
	}
	if !seen[SIDAdministrators] || !seen[SIDSystem] {
		return "в списке доступа нет записи для администраторов или системы"
	}
	return ""
}
