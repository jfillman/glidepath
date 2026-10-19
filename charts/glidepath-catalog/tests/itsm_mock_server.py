#!/usr/bin/env python3
"""A throwaway change tool for tests and the lab: the few ServiceNow Table API calls files/lib/itsm.sh makes on
change_request, plus the webhook adapter contract, over one in-memory store (docs/admin/change-management.md).

  python3 itsm_mock_server.py PORT

ServiceNow surface (what the servicenow provider calls):
  POST  /api/now/table/change_request                       create: {"result": record}; state -5 (New), approval
                                                             "not requested", number CHG0010001...
  GET   /api/now/table/change_request/<sys_id>              {"result": record}
  GET   /api/now/table/change_request?sysparm_query=number=CHG...   {"result": [record]}
  PATCH /api/now/table/change_request/<sys_id>              merges the fields; {"result": record}
Webhook surface (what the webhook provider calls): POST /webhook with {"action": create|get|link|implement|note|
  close, ...} answering {"id","number","url","state","approval","approved"} - the adapter's own judgement of
  approved is: approval == "approved", or state scheduled (-2) / implement (-1).
Test controls (never on a real instance):
  POST  /_mock/approve/<sys_id or number>                   approval "approved", state -2 (Scheduled)
  GET   /_mock/changes                                      every record, for assertions
  GET   /healthz
Auth: MOCK_TOKEN (bearer) or MOCK_USERNAME + MOCK_PASSWORD (basic) in the environment make every /api and /webhook
call require it; unset, anything goes (the lab's throwaway endpoint).
"""
import base64
import json
import os
import sys
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

STORE = {}
SEQ = [10000]
APPROVED_STATES = {"-2", "-1"}


def new_record(fields):
    SEQ[0] += 1
    rec = {
        "sys_id": uuid.uuid4().hex,
        "number": f"CHG{SEQ[0]:07d}",
        "state": "-5",
        "approval": "not requested",
        "type": "normal",
        "short_description": "",
        "description": "",
        "close_code": "",
        "close_notes": "",
        "work_notes": "",
        "parent": "",
        "correlation_id": "",
        "correlation_display": "",
    }
    rec.update({k: str(v) for k, v in fields.items()})
    STORE[rec["sys_id"]] = rec
    return rec


def find(key):
    if key in STORE:
        return STORE[key]
    for rec in STORE.values():
        if rec["number"] == key:
            return rec
    return None


def contract(rec, base):
    return {
        "id": rec["sys_id"],
        "number": rec["number"],
        "url": f"{base}/change/{rec['number']}",
        "state": rec["state"],
        "approval": rec["approval"],
        "approved": rec["approval"] == "approved" or rec["state"] in APPROVED_STATES,
    }


def append_note(rec, note):
    if note:
        rec["work_notes"] = (rec["work_notes"] + "\n" + note).strip()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # one line per call, on stderr, for the test log
        sys.stderr.write("%s %s\n" % (self.command, self.path))

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n) or b"{}") if n else {}

    def _authorized(self):
        token, user, pw = os.environ.get("MOCK_TOKEN"), os.environ.get("MOCK_USERNAME"), os.environ.get("MOCK_PASSWORD")
        if not token and not user:
            return True
        auth = self.headers.get("Authorization", "")
        if token and auth == f"Bearer {token}":
            return True
        if user and auth.startswith("Basic "):
            try:
                return base64.b64decode(auth[6:]).decode() == f"{user}:{pw}"
            except Exception:
                return False
        return False

    def _base(self):
        return f"http://{self.headers.get('Host', 'mock')}"

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/healthz":
            return self._send(200, {"ok": True})
        if u.path == "/_mock/changes":
            return self._send(200, {"result": list(STORE.values())})
        if not self._authorized():
            return self._send(401, {"error": {"message": "User Not Authenticated"}})
        if u.path == "/api/now/table/change_request":
            q = parse_qs(u.query).get("sysparm_query", [""])[0]
            if q.startswith("number="):
                rec = find(q[len("number="):])
                return self._send(200, {"result": [rec] if rec else []})
            return self._send(200, {"result": list(STORE.values())})
        if u.path.startswith("/api/now/table/change_request/"):
            rec = find(u.path.rsplit("/", 1)[1])
            return self._send(200, {"result": rec}) if rec else self._send(404, {"error": {"message": "No Record found"}})
        self._send(404, {"error": "not found"})

    def do_POST(self):
        u = urlparse(self.path)
        if u.path.startswith("/_mock/approve/"):
            rec = find(u.path.rsplit("/", 1)[1])
            if not rec:
                return self._send(404, {"error": "no such change"})
            rec["approval"], rec["state"] = "approved", "-2"
            return self._send(200, {"result": rec})
        if not self._authorized():
            return self._send(401, {"error": {"message": "User Not Authenticated"}})
        body = self._body()
        if u.path == "/api/now/table/change_request":
            return self._send(201, {"result": new_record(body)})
        if u.path == "/webhook":
            action = body.get("action")
            rel = body.get("release") or {}
            if action == "create":
                kind = rel.get("kind", "promote")
                rec = new_record({
                    "type": "emergency" if kind == "rollback" else "normal",
                    "short_description": f"{'Rollback' if kind == 'rollback' else 'Release'} {rel.get('app')} {rel.get('env')}: {rel.get('image')}",
                    "correlation_id": rel.get("release_id", ""),
                    "parent": rel.get("rollback_of_change", ""),
                })
                if rel.get("pr_url"):
                    append_note(rec, f"Release PR: {rel['pr_url']}")
                return self._send(200, contract(rec, self._base()))
            rec = find(body.get("change_id", ""))
            if not rec:
                return self._send(404, {"error": "no such change"})
            if action == "link":
                append_note(rec, f"Release PR: {rel.get('pr_url', '')}")
            elif action == "implement":
                rec["state"] = "-1"
                append_note(rec, body.get("note", ""))
            elif action == "note":
                append_note(rec, body.get("note", ""))
            elif action == "close":
                rec["state"], rec["close_code"], rec["close_notes"] = "3", body.get("close_code", ""), body.get("note", "")
                related = body.get("related_change_id", "")
                append_note(rec, body.get("note", "") + (f" (related change: {related})" if related else ""))
            elif action != "get":
                return self._send(400, {"error": f"unknown action {action}"})
            return self._send(200, contract(rec, self._base()))
        self._send(404, {"error": "not found"})

    def do_PATCH(self):
        u = urlparse(self.path)
        if not self._authorized():
            return self._send(401, {"error": {"message": "User Not Authenticated"}})
        if u.path.startswith("/api/now/table/change_request/"):
            rec = find(u.path.rsplit("/", 1)[1])
            if not rec:
                return self._send(404, {"error": {"message": "No Record found"}})
            body = self._body()
            note = body.pop("work_notes", "")
            rec.update({k: str(v) for k, v in body.items()})
            append_note(rec, note)
            return self._send(200, {"result": rec})
        self._send(404, {"error": "not found"})


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8080
    print(f"itsm mock listening on :{port}", file=sys.stderr, flush=True)
    ThreadingHTTPServer(("", port), Handler).serve_forever()
