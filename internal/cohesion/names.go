package cohesion

import "strings"

// GeneratedClientName is the name an app gives itself when the user didn't
// type one: "<device> (<client>)", or "cohesion (<client>)" with no device
// name. It mirrors cohesion-rs's `generated_client_name`.
func GeneratedClientName(device, client string) string {
	device = strings.TrimSpace(device)
	if device == "" {
		device = "cohesion"
	}
	return device + " (" + client + ")"
}
