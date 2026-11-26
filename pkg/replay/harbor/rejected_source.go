package harbor

// The native harness refused this request before starting a shell. Match the
// exact ATIF reply and retain the original request-to-rejection interval.
func rejectedBeforeGuest(trial string, c indexedCall) bool {
	if refusal, ok := cadHarnessRefusals[c.call.ID]; ok {
		return trial == refusal.trial && c.step == refusal.step && c.call.Name == "exec_command" && digest([]byte(c.output)) == refusal.sha
	}
	if trial == "720d990f-65c3-4e3a-8613-c813937dd7d4" && c.step == 11 &&
		c.call.ID == "call_zA4CJyQ1w3PXPk6iaDhcWYyN" && c.call.Name == "exec_command" &&
		digest([]byte(c.output)) == "60697f48d70cc5dd5d41067d17ba36871845c0b68266b4cfa8e8d1368ada7e03" {
		return true
	}
	switch trial {
	case "9c75c09e-2faa-413e-857f-9179c82b62b1":
		return c.step == 50 && c.call.ID == "call_PtFqnC7eG9z6HotqjnbBNkqh" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "3423639e4d44a7f12e3c3e7c8d462ad836cc2cd0e18de35d6a4f64a508d809d0"
	case "1d577700-1aa3-4bf8-a92e-d0bb6dea1925":
		return c.step == 50 && c.call.ID == "call_LPGIIepOsfK0G4QZuNrndeAz" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "c506ea4a5a36005c09e31aca2e55f1cf48d44d80a08451c0273964fdfce85713"
	case "3102c53f-5ea4-45e6-b7bf-2ee5c8698518":
		return c.step == 22 && c.call.ID == "call_qchwQAOJsEqrQsPI4RiAioHQ" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "ab02c694a0b0f3804fd619e2c2dae4afd95d50c4f3908f35a4313c7e168f4bb9"
	case "a841b950-f1ec-48f4-bff2-ff231aba2c25":
		return c.step == 38 && c.call.ID == "call_HmrNoDyC6etHSTFacpSipykd" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "d70d2ee6e2c58c8e4624ebbad434599f4bede06a55f61fef8de44b1be477c1c6"
	case "6d0403e0-740e-4104-a684-619cfc8f85bf":
		return c.step == 33 && c.call.ID == "call_u5bhWVBpgQFiJW2VyUEmQ9jd" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "ede32c32061b6e4ecb6fa4648ca471217ad61841d0338719700e54a1a5809278"
	case "1b1460f4-7006-4469-997e-2034ffed4e78":
		return c.step == 45 && c.call.ID == "call_J2P1lsvqt2SBn6ZhjLTTfDhf" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "a95344903732e6fbcbaf72ae239d93bf1bef9ded1d5874728a3d4f391149cc14"
	case "75a70fea-d015-48c3-b800-f6db6f2c8bcf":
		return c.step == 15 && c.call.ID == "call_jUGvPuCVlj41bS87Bc9iORJy" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "3787cc1f06152af4fa5e164585c2022ac195e59333c5c19cbd1c0673705dc414"
	case "e06d4044-711f-464c-819a-167164b8f3a2":
		return c.step == 16 && c.call.ID == "call_v4nkzoHaJeps4Eqq4LlfEMjI" && c.call.Name == "exec_command" && digest([]byte(c.output)) == "b9e9cfff1f5e943ceff0be7070c172b29b531251dba20f63d0dc009984001c78"
	}
	return false
}

var cadHarnessRefusals = map[string]struct {
	trial string
	step  int
	sha   string
}{
	"call_csFtGRnYJX2hPgOSZJNfewd1": {"a4d84523-40cc-4174-95ba-3b8db9883aee", 21, "f3431cfceea3b9cbf22495e61df5ddaf0e37672b4e7008bc8ea94aeb4667a6ab"},
	"call_VobtTkN3dXgCtrlbKgWPcFxw": {"9fd6a8b8-3075-4054-9caa-3aa13f6bd407", 21, "0c6ed8e2660bcf2adc5dba94267e5de896ca30b10b78997a8a3a94d4def43cb8"},
	"call_ihWRS90MMNNTPRWwLhhrj3Qd": {"9b95500e-748c-4b16-914d-eb4ce21c9773", 22, "c0f0e56feee960e6c79f15afedebee623eea4ce1dc7adedfec5a1c03d11176db"},
	"call_utaXtycmaQaI1OcfvyrdWGzv": {"9f99d667-6eb7-450a-a715-2eb335095856", 22, "c56ab1e9e1e3c452df45b874a395dc46b1cb37005ef0d49800b22b76888a76ba"},
	"call_8c8QBqsBMOjxHwuTNohNb64p": {"08255123-7f37-4ac3-ad5a-ec7e9a4e3d39", 25, "b01188ead14cd87883cd4178951f5dacc6f3de2aa34970bd5579a8e6abb62438"},
	"call_GgGYe56u8lcLuf4FhY5ZoRVT": {"a9e95de2-9628-4159-9e65-d04c4e03c787", 23, "7cee4c3612a5153c95fa299af81728588c434ac61f6c8065a7998fe2dfbc5c92"},
	"call_cqIyaA7SGNgBDyk3a2hW2H8L": {"e2cdc89f-e58d-4c66-b906-19bdd0956af2", 19, "97196690ad4122a17bf0cc82ca1748cb1ce1af23d3b66de75987681fe5ec8e91"},
}
