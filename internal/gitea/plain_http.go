package gitea

import (
	"net/url"
	"strings"
)

// PlainHTTPWarning returns a startup warning when base is plain http, where the
// token travels unencrypted. It is a warning only: a LAN-only Gitea may be http.
func PlainHTTPWarning(base string) string {
	u, err := url.Parse(base)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return ""
	}
	return "警告: Giteaの接続先（" + u.Host + "）は暗号化されないhttpです。トークンとGitの通信が平文で流れるため、信頼できるネットワークの中だけで使ってください"
}
