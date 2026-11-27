"""Forward pinned OpenClaw search calls to Brave without giving guests a provider key."""
import argparse
import hashlib
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import HTTPRedirectHandler, Request, build_opener


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def load_calls(path, expected):
    data = path.read_bytes()
    if hashlib.sha256(data).hexdigest() != expected:
        raise ValueError("trajectory digest mismatch")
    calls = {}
    for line in data.splitlines():
        message = json.loads(line).get("message", {})
        content = message.get("content")
        if message.get("role") != "assistant" or not isinstance(content, list):
            continue
        for item in content:
            if item.get("type") != "toolCall" or item.get("name") != "web_search":
                continue
            args = item["arguments"]
            keys = {"query": "q", "count": "count", "country": "country", "language": "search_lang",
                    "search_lang": "search_lang", "ui_lang": "ui_lang"}
            if set(args) - keys.keys() or not args.get("query") or item["id"] in calls:
                raise ValueError("unsupported or duplicated source search")
            params = {keys[key]: value for key, value in args.items()}
            if "search_lang" in args:
                params["search_lang"] = args["search_lang"]
            params.setdefault("count", 5)
            calls[item["id"]] = params
    if not calls:
        raise ValueError("no original search calls")
    return calls


class Search(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_POST(self):
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if self.path != "/search" or not 0 < size <= 4096:
                return self.send_error(400)
            item = json.loads(self.rfile.read(size))
            if set(item) != {"source_sha256", "call_id"} or item["source_sha256"] != self.server.digest:
                return self.send_error(403)
            params = self.server.calls[item["call_id"]]
        except (ValueError, TypeError, KeyError):
            return self.send_error(400)
        req = Request("https://api.search.brave.com/res/v1/web/search?" + urlencode(params),
                      headers={"Accept": "application/json", "X-Subscription-Token": self.server.token})
        try:
            with build_opener(NoRedirect).open(req, timeout=45) as response:
                data = response.read()
        except HTTPError as err:
            return self.send_error(err.code)
        except (TimeoutError, URLError):
            return self.send_error(502)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


if __name__ == "__main__":
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("trace", type=Path)
    cli.add_argument("digest")
    cli.add_argument("key_file", type=Path)
    cli.add_argument("--port", type=int, default=0)
    args = cli.parse_args()
    with ThreadingHTTPServer(("127.0.0.1", args.port), Search) as server:
        server.digest = args.digest
        server.calls = load_calls(args.trace, args.digest)
        server.token = args.key_file.read_text().strip()
        if server.token.startswith(("BRAVE_API_KEY=", "BRAVE_SEARCH_API_KEY=")):
            server.token = server.token.split("=", 1)[1].strip().strip("'\"")
        if not server.token or any(ch.isspace() for ch in server.token):
            raise ValueError("invalid provider key file")
        print(server.server_port, flush=True)
        server.serve_forever()
