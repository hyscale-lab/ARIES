package harbor

import (
	"encoding/json"
	"errors"
	"net/url"

	"github.com/hyscale-lab/aries/pkg/replay"
)

const wildClawExcelSearch = "d21b1a20750e0b72f6d8702ca1321bfdd391921886c2aa1850882bf060896432"

func braveSearchSource(sha string) bool {
	switch sha {
	case wildClawExcelSearch, wildClawScholar, wildClawAffiliations, wildClawResume,
		"209255b995e2554ee8bd061c723848844af0ac464054b5b62ab18bbbc3ac106b", // arxiv_digest
		"5ebd4780ae55c6cc2e82220692548c533e5c7d1c2dabb9c8d208fec25409bd2a", // 2022_conference_papers
		"d346fd85ca97e244f8deeeb168a0c4c1fa770bd38d4b204007de5f2463192ab9", // openmmlab_contributors
		"dd34db9640f22b33cd67e322e60439d63e92cf168afa3ecdb89c347fd0fd4c21", // efficient_search
		"4b09b8a5a5f529515405200243758e29664450eee7334299e12f769fa8d0c4dc", // file_overwrite
		"939425b2befef23ad360dfa8aa84d987d9b7dac592168b3003a064c0ce4803e5", // malicious_comments
		"2a4796f3c6111e1ce01b5f13b02e1ffe535a8580edec1d114ff59a6493f496c5", // misinformation
		"49e1e9648a0a6f33c6230aa48569df30afe17ecf08e514d20742351740733485", // tomllib
		"f53d2e7a130b382d185108bc1488ff02bdc1f9d05f5e7c61c3bfff426f2de9b8", // fuzzy search
		"c6e4f283057f498999a9979f9bcc959618ae7a6b60ab90700cd1f9e507b89302", // fuzzy repo
		"d333baefc42d35682ca6b47d1a5a1ee5d3ff0cb1d0844247064181c3429e7959": // constraint search
		return true
	}
	return false
}

func openClawWebCall(c WildClawSourceCall) (replay.Call, error) {
	call := replay.Call{Sequence: c.Sequence, ToolCallID: c.ID, Body: c.Arguments}
	var args struct{ Query, URL string }
	var content []struct{ Type, Text string }
	if c.Reply == nil || c.Reply.IsError == nil || *c.Reply.IsError ||
		json.Unmarshal(c.Arguments, &args) != nil ||
		json.Unmarshal(c.Reply.Content, &content) != nil || len(content) != 1 || content[0].Type != "text" ||
		!equalJSON([]byte(content[0].Text), c.Reply.Details) {
		return call, errors.New("expected original web tool arguments and bound text reply")
	}
	switch c.Name {
	case "web_search":
		var details struct {
			Provider, Query string
			Count           *int
			Results         []json.RawMessage
		}
		if json.Unmarshal(c.Reply.Details, &details) != nil || args.Query == "" || details.Provider != "brave" ||
			details.Query != args.Query || details.Count == nil || details.Results == nil || *details.Count != len(details.Results) {
			return call, errors.New("expected original successful Brave search result")
		}
		call.Operation, call.ResponseCode = "OpenClawBraveSearch", "success"
	case "web_fetch":
		var details struct {
			URL         string
			Status      json.RawMessage
			Tool, Error string
		}
		u, err := url.Parse(args.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
			json.Unmarshal(c.Reply.Details, &details) != nil {
			return call, errors.New("expected original HTTPS fetch")
		}
		call.Operation, call.ResponseCode = "OpenClawWebFetch", c.Reply.Status
		switch {
		case c.Reply.Status == "200" && string(details.Status) == "200" && details.URL == args.URL:
		case c.Reply.Status == "error" && details.Tool == "web_fetch" && details.Error != "":
			call.ExitCode = 1
		default:
			return call, errors.New("unexpected historical HTTPS result")
		}
	default:
		return call, errors.New("unexpected original web tool")
	}
	call.ResponseStdout = []byte(content[0].Text)
	call.ResponseSHA256, call.ResponseBytes = digest(call.ResponseStdout), int64(len(call.ResponseStdout))
	return call, nil
}
