package cli

import (
	"strings"

	"github.com/DrCuper/amneziawg-keenos-auto-connector/internal/router"
)

// All mutating commands use the one-line form so that each command is
// self-contained: both `POST /rci/ [{"parse": ...}]` and an SSH exec session
// are stateless between commands, so the "enter section, then type sub-command"
// form would not work.

// CreateGroup returns the command that creates an (empty) FQDN group.
func CreateGroup(name string) string { return "object-group fqdn " + name }

// DeleteGroup removes a group with all its entries.
func DeleteGroup(name string) string { return "no object-group fqdn " + name }

// SetDescription sets the group description.
func SetDescription(name, desc string) string {
	return "object-group fqdn " + name + " description " + Quote(desc)
}

// ClearDescription removes the group description.
func ClearDescription(name string) string {
	return "no object-group fqdn " + name + " description"
}

// Include / Exclude add an entry; NoInclude / NoExclude remove it.
func Include(name, entry string) string { return "object-group fqdn " + name + " include " + entry }
func NoInclude(name, entry string) string {
	return "no object-group fqdn " + name + " include " + entry
}
func Exclude(name, entry string) string { return "object-group fqdn " + name + " exclude " + entry }
func NoExclude(name, entry string) string {
	return "no object-group fqdn " + name + " exclude " + entry
}

// AddRoute binds a group to an interface/gateway via dns-proxy.
func AddRoute(r router.DNSRoute) string {
	s := "dns-proxy route object-group " + r.Group + " " + r.Destination()
	if r.Auto {
		s += " auto"
	}
	if r.Reject {
		s += " reject"
	}
	return s
}

// RemoveRoute unbinds a group from a destination.
func RemoveRoute(r router.DNSRoute) string {
	return "no dns-proxy route object-group " + r.Group + " " + r.Destination()
}

// Save persists running-config to startup-config.
const Save = "system configuration save"

// ShowRunningConfig is the read command used by the SSH transport.
const ShowRunningConfig = "show running-config"

// Quote wraps s in double quotes when it contains whitespace or is empty.
func Quote(s string) string {
	if s == "" || strings.ContainsAny(s, " \t") {
		return `"` + strings.ReplaceAll(s, `"`, `'`) + `"`
	}
	return s
}
