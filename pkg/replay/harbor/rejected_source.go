package harbor

// The native harness refused this request before starting a shell. Match the
// exact ATIF reply and retain the original request-to-rejection interval.
func rejectedBeforeGuest(trial string, c indexedCall) bool {
	return trial == "720d990f-65c3-4e3a-8613-c813937dd7d4" && c.step == 11 &&
		c.call.ID == "call_zA4CJyQ1w3PXPk6iaDhcWYyN" && c.call.Name == "exec_command" &&
		digest([]byte(c.output)) == "60697f48d70cc5dd5d41067d17ba36871845c0b68266b4cfa8e8d1368ada7e03"
}
