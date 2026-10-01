"""Per-attempt Responses budget gateway for the isolated live evaluation runner.

This is a deliberately narrow protocol adapter, not a general OpenAI proxy.
Every generation first buys a bounded input-count request and then reserves its
full counted input plus maximum output. Reservations are never refunded, even
when the client disconnects or the provider fails. Restarting a gate creates a
new budget: the runner must prohibit resuming/repeating an attempt.

Prices are supplied by the approved plan. The input rate must upper-bound every
applicable input class (including cache writes), and count_price_per_request
must explicitly cover the provider's counting endpoint. No pricing is inferred.
"""
from __future__ import annotations

from dataclasses import dataclass
from decimal import Decimal
import hmac
import http.client
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import math
import socket
import threading
import time
from urllib.parse import urlsplit

import budget


@dataclass(frozen=True)
class Config:
    model: str
    provider_url: str
    input_price_per_million: Decimal | str
    output_price_per_million: Decimal | str
    count_price_per_request: Decimal | str
    total_tokens: int
    max_output_tokens: int
    max_cost_usd: Decimal | str
    max_requests: int = 100
    timeout_seconds: float = 60
    max_request_bytes: int = 4 * 1024 * 1024
    max_response_bytes: int = 8 * 1024 * 1024

    @budget.exact
    def __post_init__(self):
        if not isinstance(self.model, str) or not self.model.strip() or len(self.model) > 256:
            raise ValueError("a fixed model is required")
        parsed = urlsplit(self.provider_url)
        if (parsed.scheme != "https" or not parsed.hostname or parsed.username is not None
                or parsed.password is not None or parsed.query or parsed.fragment
                or any(c.isspace() or ord(c) < 32 for c in self.provider_url)):
            raise ValueError("provider_url must be an explicit HTTPS base URL without credentials/query")
        try:
            parsed.port
        except ValueError as exc:
            raise ValueError("invalid provider port") from exc
        for name in ("input_price_per_million", "output_price_per_million", "count_price_per_request", "max_cost_usd"):
            value = budget.amount(getattr(self, name), zero=name == "count_price_per_request", computed=name == "max_cost_usd")
            object.__setattr__(self, name, value)
        for name in ("total_tokens", "max_output_tokens", "max_requests", "max_request_bytes", "max_response_bytes"):
            value = getattr(self, name)
            budget.count(value)
        if self.max_output_tokens < 16 or self.max_output_tokens > self.total_tokens:
            raise ValueError("max_output_tokens must be at least 16 and within total_tokens")
        if isinstance(self.timeout_seconds, bool) or not math.isfinite(self.timeout_seconds) or not 0 < self.timeout_seconds <= 600:
            raise ValueError("timeout_seconds must be in (0, 600]")
        if self.max_requests > 10000 or max(self.max_request_bytes, self.max_response_bytes) > 64 * 1024 * 1024:
            raise ValueError("request/byte limits exceed gateway safety ceilings")


class _Rejected(Exception):
    def __init__(self, code: str, status: int = 400):
        self.code, self.status = code, status


class _HTTPTransport:
    """No environment proxies, redirects, retries, cookies or implicit auth.

    The injected HTTP variant is only for offline tests. Production construction
    accepts HTTPS and the host-owned key; neither is client configurable.
    """
    def __init__(self, base_url, key, *, _allow_http=False):
        parsed = urlsplit(base_url)
        if parsed.scheme != "https" and not (_allow_http and parsed.scheme == "http"):
            raise ValueError("HTTPS upstream required")
        self._url, self._key = parsed, key
        self._lock, self._active, self._socket, self._closed = threading.Lock(), None, None, False

    def close(self):
        with self._lock:
            self._closed = True
            if self._socket is not None:
                try:
                    self._socket.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            if self._active is not None:
                self._active.close()

    def __call__(self, path, payload, max_bytes, deadline):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("deadline")
        cls = http.client.HTTPSConnection if self._url.scheme == "https" else http.client.HTTPConnection
        connection = cls(self._url.hostname, self._url.port, timeout=remaining)
        with self._lock:
            if self._closed:
                raise OSError("closed")
            self._active = connection
        # Closing the socket aborts a stalled header/body even when data trickles.
        # DNS resolution belongs to the host resolver; no new generation is sent
        # after it returns beyond the deadline (checked before request()).
        active_socket = None
        def abort():
            if active_socket is not None:
                try:
                    active_socket.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
            connection.close()
        timer = threading.Timer(remaining, abort)
        timer.daemon = True
        timer.start()
        try:
            connection.connect()
            active_socket = connection.sock
            with self._lock:
                self._socket = active_socket
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError("deadline")
            with self._lock:
                if self._closed:
                    raise OSError("closed")
            connection.sock.settimeout(remaining)
            connection.request("POST", self._url.path.rstrip("/") + path, body=payload,
                               headers={"Authorization": "Bearer " + self._key,
                                        "Content-Type": "application/json", "Accept": "application/json, text/event-stream"})
            response = connection.getresponse()
            parts, length = [], 0
            while True:
                if response.length == 0:
                    break
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("deadline")
                active_socket.settimeout(remaining)
                chunk = response.read1(min(65536, max_bytes + 1 - length))
                if not chunk:
                    if response.length not in (None, 0):
                        raise http.client.IncompleteRead(b"", response.length)
                    break
                parts.append(chunk)
                length += len(chunk)
                if length > max_bytes:
                    raise _Rejected("upstream_response_too_large", 502)
            return response.status, response.getheader("Content-Type", ""), b"".join(parts)
        finally:
            timer.cancel()
            connection.close()
            with self._lock:
                self._active, self._socket = None, None


def _decode(body):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate JSON key")
            result[key] = value
        return result
    return json.loads(body, object_pairs_hook=unique,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def _validate_input(value):
    if isinstance(value, str):
        return
    if not isinstance(value, list):
        raise _Rejected("unsupported_input")
    for item in value:
        if not isinstance(item, dict):
            raise _Rejected("unsupported_input")
        kind = item.get("type", "message")
        if kind == "message":
            if set(item) - {"type", "role", "content", "id", "status", "phase"} or item.get("role") not in {"user", "assistant", "system", "developer"}:
                raise _Rejected("unsupported_message")
            if item.get("phase") not in (None, "commentary", "final_answer"):
                raise _Rejected("unsupported_message_phase")
            content = item.get("content")
            if isinstance(content, str):
                continue
            if not isinstance(content, list):
                raise _Rejected("text_input_required")
            for part in content:
                if (not isinstance(part, dict) or part.get("type") not in {"input_text", "output_text"}
                        or not isinstance(part.get("text"), str)
                        or set(part) - {"type", "text", "annotations", "logprobs"}):
                    raise _Rejected("text_input_required")
                if part.get("annotations") or part.get("logprobs"):
                    raise _Rejected("unsupported_text_metadata")
        elif kind == "function_call":
            if (set(item) - {"type", "id", "call_id", "name", "arguments", "status"}
                    or any(not isinstance(item.get(key), str) for key in ("call_id", "name", "arguments"))):
                raise _Rejected("unsupported_function_input")
        elif kind == "function_call_output":
            if (set(item) - {"type", "id", "call_id", "output", "status"}
                    or any(not isinstance(item.get(key), str) for key in ("call_id", "output"))):
                raise _Rejected("unsupported_function_output")
        elif kind == "reasoning":
            if set(item) - {"type", "id", "summary", "encrypted_content", "status"}:
                raise _Rejected("unsupported_reasoning_input")
            summary = item.get("summary", [])
            if not isinstance(summary, list) or any(not isinstance(part, dict) or set(part) != {"type", "text"}
                    or part["type"] != "summary_text" or not isinstance(part["text"], str) for part in summary):
                raise _Rejected("unsupported_reasoning_input")
            if "encrypted_content" in item and not isinstance(item["encrypted_content"], str):
                raise _Rejected("unsupported_reasoning_input")
        else:
            raise _Rejected("unsupported_input_type")


def _generation(payload, config):
    if not isinstance(payload, dict) or payload.get("model") != config.model:
        raise _Rejected("unapproved_model")
    allowed = {"model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "stream",
               "max_output_tokens", "store", "service_tier", "truncation", "reasoning", "text", "temperature", "top_p"}
    if set(payload) - allowed:
        raise _Rejected("unsupported_request_field")
    _validate_input(payload.get("input"))
    if "instructions" in payload and not isinstance(payload["instructions"], str):
        raise _Rejected("unsupported_instructions")
    tools = payload.get("tools", [])
    if not isinstance(tools, list):
        raise _Rejected("function_tools_required")
    names = set()
    for tool in tools:
        if (not isinstance(tool, dict) or tool.get("type") != "function"
                or set(tool) - {"type", "name", "description", "parameters", "strict"}
                or not isinstance(tool.get("name"), str) or not tool["name"] or tool["name"] in names):
            raise _Rejected("function_tools_required")
        names.add(tool["name"])
    choice = payload.get("tool_choice", "auto")
    if not (isinstance(choice, str) and choice in {"auto", "none", "required"}):
        if not isinstance(choice, dict) or set(choice) != {"type", "name"} or choice.get("type") != "function" or choice.get("name") not in names:
            raise _Rejected("function_tool_choice_required")
    for key in ("stream", "parallel_tool_calls", "store"):
        if key in payload and type(payload[key]) is not bool:
            raise _Rejected("invalid_boolean_option")
    if payload.get("service_tier", "default") != "default" or payload.get("truncation", "disabled") != "disabled":
        raise _Rejected("unsupported_billing_or_truncation")
    if "reasoning" in payload:
        reasoning = payload["reasoning"]
        if (not isinstance(reasoning, dict) or set(reasoning) - {"effort", "summary"}
                or reasoning.get("effort", "medium") not in {"none", "minimal", "low", "medium", "high", "xhigh"}
                or reasoning.get("summary", "auto") not in {"auto", "concise", "detailed"}):
            raise _Rejected("unsupported_reasoning")
    # No audio/image/file input, hosted tools, cache mode, custom tier, background
    # operation, external conversation state, structured external schemas, etc.
    if "text" in payload and payload["text"] not in ({}, {"format": {"type": "text"}}):
        raise _Rejected("unsupported_text_option")
    for name in ("temperature", "top_p"):
        if name in payload and (type(payload[name]) not in (int, float) or not math.isfinite(payload[name])):
            raise _Rejected("invalid_sampling_option")
    result = dict(payload)
    result.update(max_output_tokens=config.max_output_tokens, store=False, service_tier="default", truncation="disabled",
                  include=["reasoning.encrypted_content"])
    return result


def _usage(body, content_type):
    """Reported usage is evidence only; it never releases a reservation."""
    if content_type.split(";")[0].strip().lower() == "text/event-stream":
        objects = []
        for block in body.replace(b"\r\n", b"\n").split(b"\n\n"):
            data = b"\n".join(line[5:].lstrip() for line in block.split(b"\n") if line.startswith(b"data:"))
            if data and data != b"[DONE]":
                objects.append(_decode(data))
        terminal = [item.get("response") for item in objects if isinstance(item, dict)
                    and item.get("type") in {"response.completed", "response.incomplete", "response.failed"}]
        if len(terminal) != 1:
            raise ValueError("missing unique terminal response")
        response = terminal[0]
    elif content_type.split(";")[0].strip().lower() == "application/json":
        response = _decode(body)
    else:
        raise ValueError("unsupported response type")
    if not isinstance(response, dict) or response.get("status") not in {"completed", "incomplete", "failed", "cancelled"}:
        raise ValueError("missing terminal response")
    usage = response.get("usage")
    if not isinstance(usage, dict) or any(type(usage.get(key)) is not int or usage[key] < 0 for key in ("input_tokens", "output_tokens")):
        return response["status"], None
    return response["status"], {key: usage[key] for key in ("input_tokens", "output_tokens")}


class BudgetGateway:
    def __init__(self, config: Config, *, upstream_key: str, bearer_nonce: str,
                 bind_host="127.0.0.1", bind_port=0, _transport=None):
        if not isinstance(config, Config):
            raise ValueError("validated Config required")
        for value in (upstream_key, bearer_nonce):
            if not isinstance(value, str) or not 16 <= len(value) <= 4096 or any(ord(c) < 33 or ord(c) > 126 for c in value):
                raise ValueError("nonempty printable credentials of at least 16 characters required")
        if hmac.compare_digest(upstream_key, bearer_nonce):
            raise ValueError("agent nonce must differ from provider key")
        self.config, self._key, self._nonce = config, upstream_key, bearer_nonce
        self._bind = (bind_host, bind_port)
        self._transport_mode = "injected" if _transport is not None else "https"
        self._transport = _transport if _transport is not None else _HTTPTransport(config.provider_url, upstream_key)
        self._lock, self._closed = threading.RLock(), threading.Event()
        self._started, self._server, self._thread, self._client = False, None, None, None
        self._attempts, self._tokens, self._cost = 0, 0, Decimal(0)
        self._records, self._halted = [], False

    @property
    def address(self):
        if self._server is None:
            raise RuntimeError("gateway is not running")
        return self._server.server_address

    @property
    def base_url(self):
        host, port = self.address
        return f"http://{host}:{port}/v1"

    def __enter__(self):
        if self._started or self._closed.is_set():
            raise RuntimeError("an attempt gateway cannot restart")
        self._started = True
        gate = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"
            server_version = "MeldraBudgetGateway"
            sys_version = ""

            def setup(self):
                super().setup()
                self.connection.settimeout(gate.config.timeout_seconds)
                gate._client = self.connection
                self._timer = threading.Timer(gate.config.timeout_seconds, self._abort)
                self._timer.daemon = True
                self._timer.start()

            def _abort(self):
                try:
                    self.connection.shutdown(socket.SHUT_RDWR)
                except OSError:
                    pass
                self.connection.close()

            def finish(self):
                self._timer.cancel()
                gate._client = None
                super().finish()

            def log_message(self, *args):
                pass

            def handle(self):
                try:
                    super().handle()
                except (ConnectionError, OSError, ValueError):
                    pass

            def do_POST(self):
                try:
                    auth = self.headers.get_all("Authorization", [])
                    expected = ("Bearer " + gate._nonce).encode()
                    if len(auth) != 1 or not hmac.compare_digest(auth[0].encode(), expected):
                        raise _Rejected("unauthorized", 401)
                    if self.path != "/v1/responses":
                        raise _Rejected("unsupported_route", 404)
                    lengths = self.headers.get_all("Content-Length", [])
                    if self.headers.get("Transfer-Encoding") or len(lengths) != 1 or not lengths[0].isascii() or not lengths[0].isdigit():
                        raise _Rejected("content_length_required", 411)
                    size = int(lengths[0])
                    if size > gate.config.max_request_bytes:
                        raise _Rejected("request_too_large", 413)
                    if self.headers.get("Content-Type", "").split(";")[0].strip().lower() != "application/json":
                        raise _Rejected("json_required", 415)
                    body = self.rfile.read(size)
                    if len(body) != size:
                        raise _Rejected("incomplete_request")
                    try:
                        payload = _generation(_decode(body), gate.config)
                    except (ValueError, RecursionError, UnicodeError, TypeError):
                        raise _Rejected("invalid_request_json") from None
                    status, content_type, response = gate._admit(payload)
                    self._reply(status, content_type, response)
                except _Rejected as exc:
                    self._reply(exc.status, "application/json", json.dumps({"error": {"type": "budget_gateway", "code": exc.code}}).encode())
                except (OSError, ValueError, RecursionError):
                    self._reply(502, "application/json", b'{"error":{"type":"budget_gateway","code":"request_failed"}}')

            def _reply(self, status, content_type, body):
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", content_type.split(";")[0])
                    self.send_header("Content-Length", str(len(body)))
                    self.send_header("Cache-Control", "no-store")
                    self.send_header("Connection", "close")
                    self.end_headers()
                    self.wfile.write(body)
                except (OSError, ValueError):
                    pass

            def send_error(self, code, message=None, explain=None):
                self._reply(code, "application/json", b'{"error":{"type":"budget_gateway","code":"unsupported_http"}}')

        self._server = HTTPServer(self._bind, Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *args):
        self._closed.set()
        close = getattr(self._transport, "close", None)
        if close is not None:
            close()
        if self._client is not None:
            try:
                self._client.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
            self._client.close()
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
            self._thread.join(timeout=self.config.timeout_seconds + 1)

    @budget.exact
    def _admit(self, payload):
        with self._lock:
            if self._closed.is_set() or self._halted:
                raise _Rejected("gateway_closed", 409)
            generation_body = json.dumps(payload, allow_nan=False).encode()
            count = {key: value for key, value in payload.items()
                     if key in {"model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text", "truncation"}}
            count_body = json.dumps(count, allow_nan=False).encode()
            if max(len(generation_body), len(count_body)) > self.config.max_request_bytes:
                raise _Rejected("request_too_large", 413)
            if self._attempts >= self.config.max_requests:
                raise _Rejected("request_budget_exhausted", 402)
            if self._tokens + self.config.max_output_tokens > self.config.total_tokens:
                raise _Rejected("token_budget_exhausted", 402)
            if self._cost + self.config.count_price_per_request > self.config.max_cost_usd:
                raise _Rejected("cost_budget_exhausted", 402)
            self._attempts += 1
            self._cost += self.config.count_price_per_request
            record = {"request": self._attempts, "status": "count_reserved", "count_fee_usd": str(self.config.count_price_per_request),
                      "reserved_input_tokens": 0, "reserved_output_tokens": 0, "reported_usage": None}
            self._records.append(record)
            deadline = time.monotonic() + self.config.timeout_seconds
            try:
                status, _, body = self._transport("/responses/input_tokens", count_body, 65536, deadline)
                if status != 200:
                    raise _Rejected("input_count_failed", 502)
                counted = _decode(body)
                if not isinstance(counted, dict) or type(counted.get("input_tokens")) is not int or not 0 <= counted["input_tokens"] <= 1_000_000_000:
                    raise _Rejected("invalid_input_count", 502)
                input_tokens = counted["input_tokens"]
            except Exception:
                record["status"] = "count_failed"
                raise _Rejected("input_count_failed", 502) from None
            reservation = input_tokens + self.config.max_output_tokens
            cost = (Decimal(input_tokens) * self.config.input_price_per_million
                    + Decimal(self.config.max_output_tokens) * self.config.output_price_per_million) / 1_000_000
            if self._tokens + reservation > self.config.total_tokens or self._cost + cost > self.config.max_cost_usd:
                record["status"] = "generation_budget_denied"
                raise _Rejected("generation_budget_exhausted", 402)
            if self._closed.is_set():
                record["status"] = "closed_after_count"
                raise _Rejected("gateway_closed", 409)
            self._tokens += reservation
            self._cost += cost
            record.update(status="generation_reserved", reserved_input_tokens=input_tokens,
                          reserved_output_tokens=self.config.max_output_tokens, generation_reservation_usd=str(cost))
            try:
                status, content_type, body = self._transport("/responses", generation_body, self.config.max_response_bytes, deadline)
                if status != 200:
                    record["status"] = "generation_http_error"
                    record["http_status"] = status
                    raise _Rejected("generation_failed", 502)
                terminal, usage = _usage(body, content_type)
                record.update(status=terminal, reported_usage=usage)
                if usage is not None and (usage["input_tokens"] > input_tokens or usage["output_tokens"] > self.config.max_output_tokens):
                    self._halted = True
                    record["status"] = "provider_exceeded_reservation"
                    raise _Rejected("provider_exceeded_reservation", 502)
                # Redact literal credential echoes; the chosen provider remains
                # trusted for billing and response confidentiality. Reports never
                # retain request/response bodies.
                body = body.replace(self._key.encode(), b"[REDACTED]").replace(self._nonce.encode(), b"[REDACTED]")
                return status, content_type, body
            except _Rejected:
                if record["status"] == "generation_reserved":
                    record["status"] = "generation_unknown"
                raise
            except Exception:
                record["status"] = "generation_unknown"
                raise _Rejected("generation_failed", 502) from None

    @budget.exact
    def report(self):
        with self._lock:
            usages = [record["reported_usage"] for record in self._records if record["reported_usage"] is not None]
            complete = bool(usages) and all(record["reported_usage"] is not None for record in self._records if record["reserved_output_tokens"])
            return {"schema_version": 1, "model": self.config.model, "provider_url": self.config.provider_url,
                    "transport_mode": self._transport_mode,
                    "requests": self._attempts, "reserved_tokens": self._tokens, "reserved_cost_usd": str(self._cost),
                    "reported_input_tokens": sum(item["input_tokens"] for item in usages) if complete else None,
                    "reported_output_tokens": sum(item["output_tokens"] for item in usages) if complete else None,
                    "usage_complete": complete,
                    "reservation_integrity": not self._halted, "closed": self._closed.is_set(),
                    "requests_detail": json.loads(json.dumps(self._records))}
