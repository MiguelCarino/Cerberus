// Package packaging holds the files used to install Cerberus as a service.
// cerberus.service is the systemd user unit that `cerberus install-service`
// writes to ~/.config/systemd/user/.
package packaging

import _ "embed"

// Unit is the systemd user unit (cerberus.service).
//
//go:embed cerberus.service
var Unit string
