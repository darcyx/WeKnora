package sandboxalloc

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/sandbox/allocation"
	"github.com/Tencent/WeKnora/internal/types"
)

func ownerFromSession(s *types.Session) allocation.Owner {
	src := allocation.AuthSource(strings.TrimSpace(s.OwnerAuthSource))
	if src == "" {
		src = allocation.SourceUnknown
	}
	o := allocation.Owner{TenantID: s.TenantID, ID: s.UserID, Kind: allocation.KindUnknown, Source: src}
	id := strings.TrimSpace(s.UserID)
	switch src {
	case allocation.SourceWeb:
		if id != "" && !strings.Contains(id, ":") {
			o.Kind = allocation.KindWebUser
		}
	case allocation.SourceAPIDirectHeader, allocation.SourceAPISignedToken:
		if strings.HasPrefix(id, types.SessionOwnerAPIExternalUserPrefix) {
			o.Kind = allocation.KindAPIExternalUser
		}
	case allocation.SourceAPITenantKey:
		o.Kind = allocation.KindAPITenantKey
	case allocation.SourceAPIPlatformKey:
		o.Kind = allocation.KindAPIPlatformKey
	case allocation.SourceMCPEndpoint:
		o.Kind = allocation.KindMCPEndpoint
	case allocation.SourceEmbed:
		o.Kind = allocation.KindEmbed
	case allocation.SourceIM:
		o.Kind = allocation.KindIM
	}
	return o
}
