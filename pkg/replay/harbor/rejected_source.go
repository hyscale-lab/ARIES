package harbor

// The native harness refused this request before starting a shell. Match the
// exact ATIF reply and retain the original request-to-rejection interval.
func rejectedBeforeGuest(trial string, c indexedCall) bool {
	if trial == "720d990f-65c3-4e3a-8613-c813937dd7d4" && c.step == 11 &&
		c.call.ID == "call_zA4CJyQ1w3PXPk6iaDhcWYyN" && c.call.Name == "exec_command" &&
		digest([]byte(c.output)) == "60697f48d70cc5dd5d41067d17ba36871845c0b68266b4cfa8e8d1368ada7e03" {
		return true
	}
	switch trial {
	case "a841b950-f1ec-48f4-bff2-ff231aba2c25":
		return c.step == 38 && c.call.ID == "call_HmrNoDyC6etHSTFacpSipykd" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "d70d2ee6e2c58c8e4624ebbad434599f4bede06a55f61fef8de44b1be477c1c6"
	case "6d0403e0-740e-4104-a684-619cfc8f85bf":
		return c.step == 33 && c.call.ID == "call_u5bhWVBpgQFiJW2VyUEmQ9jd" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "ede32c32061b6e4ecb6fa4648ca471217ad61841d0338719700e54a1a5809278"
	case "1b1460f4-7006-4469-997e-2034ffed4e78":
		return c.step == 45 && c.call.ID == "call_J2P1lsvqt2SBn6ZhjLTTfDhf" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "a95344903732e6fbcbaf72ae239d93bf1bef9ded1d5874728a3d4f391149cc14"
	}
	return false
}
