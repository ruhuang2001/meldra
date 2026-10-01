"""Offline HTTP contract tests: all upstream traffic stays on loopback."""
import concurrent.futures
from dataclasses import replace
from decimal import Decimal, ROUND_DOWN, localcontext
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
from pathlib import Path
import socket
import sys
import threading
import time
import unittest

SPEC = importlib.util.spec_from_file_location("live_budget_gate", Path(__file__).with_name("gate.py"))
gate = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = gate
SPEC.loader.exec_module(gate)

KEY = "private-provider-key-never-forward"
NONCE = "per-attempt-random-bearer-nonce"


def completed(input_tokens=10, output_tokens=7, status="completed"):
    return {"id": "resp_test", "object": "response", "model": "fixed-model", "status": status,
            "output": [], "usage": {"input_tokens": input_tokens, "output_tokens": output_tokens}}


def as_sse(response):
    kind = "response." + response["status"]
    return b"event: " + kind.encode() + b"\ndata: " + json.dumps({"type": kind, "response": response}).encode() + b"\n\n"


class FakeUpstream:
    def __init__(self):
        self.requests = []
        self.count_status, self.count_body = 200, b'{"input_tokens":10}'
        self.generation_status, self.generation_body = 200, json.dumps(completed()).encode()
        self.content_type = "application/json"
        self.delay, self.truncate, self.drip = 0, False, False
        parent = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                body = self.rfile.read(int(self.headers["Content-Length"]))
                parent.requests.append((self.path, dict(self.headers), json.loads(body)))
                is_count = self.path == "/v1/responses/input_tokens"
                if not is_count:
                    time.sleep(parent.delay)
                status = parent.count_status if is_count else parent.generation_status
                body = parent.count_body if is_count else parent.generation_body
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json" if is_count else parent.content_type)
                    if status in (301, 302, 307, 308):
                        self.send_header("Location", "/would-leak-auth")
                    self.send_header("Content-Length", str(len(body) + (100 if parent.truncate and not is_count else 0)))
                    self.end_headers()
                    if parent.drip and not is_count:
                        for character in body:
                            self.wfile.write(bytes([character]))
                            self.wfile.flush()
                            time.sleep(0.03)
                    else:
                        self.wfile.write(body)
                except (OSError, ValueError):
                    pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        self.thread.start()

    def transport(self):
        return gate._HTTPTransport(f"http://127.0.0.1:{self.server.server_port}/v1", KEY, _allow_http=True)

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(1)


class GatewayTests(unittest.TestCase):
    def setUp(self):
        self.upstream = FakeUpstream()
        self.addCleanup(self.upstream.close)
        self.config = gate.Config(model="fixed-model", provider_url="https://approved.example/v1",
                                  input_price_per_million="2", output_price_per_million="10",
                                  count_price_per_request="0.001", total_tokens=200,
                                  max_output_tokens=20, max_cost_usd="1", timeout_seconds=2)

    def gateway(self, **changes):
        return gate.BudgetGateway(replace(self.config, **changes), upstream_key=KEY,
                                  bearer_nonce=NONCE, _transport=self.upstream.transport())

    def request(self, proxy, payload=None, *, route="/v1/responses", auth=NONCE, method="POST", raw=None):
        if payload is None:
            payload = {"model": "fixed-model", "input": "repair", "stream": False}
        body = json.dumps(payload).encode() if raw is None else raw
        conn = http.client.HTTPConnection(*proxy.address, timeout=3)
        try:
            conn.request(method, route, body=body, headers={"Authorization": "Bearer " + auth,
                                                           "Content-Type": "application/json"})
            result = conn.getresponse()
            return result.status, result.read()
        finally:
            conn.close()

    def test_close_survives_handler_clearing_active_client(self):
        proxy=gate.BudgetGateway(self.config,upstream_key=KEY,bearer_nonce=NONCE,
                                 _transport=lambda *args: None)
        class FinishingClient:
            closed=False
            def shutdown(self, how):
                proxy._client=None
            def close(self):
                self.closed=True
        client=FinishingClient()
        proxy._client=client
        proxy.__exit__(None,None,None)
        self.assertTrue(client.closed)
        self.assertTrue(proxy.report()["closed"])

    def test_validated_config_requires_explicit_prices_and_https(self):
        for key, value in [("provider_url", "http://upstream/v1"), ("provider_url", "https://user:pass@host/v1"),
                           ("provider_url", "https://host/v1?key=secret"), ("provider_url", "https://host:bad/v1"),
                           ("model", ""), ("max_output_tokens", 15), ("max_output_tokens", 201),
                           ("total_tokens", True), ("total_tokens", 1_000_000_001), ("max_requests", 0), ("timeout_seconds", float("nan")),
                           ("timeout_seconds", 601), ("max_request_bytes", 65 * 1024 * 1024)]:
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                replace(self.config, **{key: value})
        for key in ("input_price_per_million", "output_price_per_million", "max_cost_usd", "count_price_per_request"):
            for value in ("unknown", "NaN", "Infinity", "-1", "1e-19" if key == "max_cost_usd" else "1e-13", "1e13", "1.123456789012345678901234"):
                with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                    replace(self.config, **{key: value})
        self.assertEqual(replace(self.config, count_price_per_request="0").count_price_per_request, Decimal(0))
        with self.assertRaises(ValueError):
            gate.BudgetGateway(self.config, upstream_key=KEY, bearer_nonce=KEY)

    def test_admission_uses_exact_arithmetic_independent_of_host_decimal_context(self):
        payload = gate._generation({"model": "fixed-model", "input": "repair"}, self.config)
        rates = {"count_price_per_request": "0.001000000001",
                 "input_price_per_million": "2.123456789012", "output_price_per_million": "10.123456789012"}
        with localcontext() as context:
            context.prec, context.rounding = 4, ROUND_DOWN
            with self.gateway(max_cost_usd="0.001223703704", **rates) as proxy:
                with self.assertRaises(gate._Rejected) as rejected:
                    proxy._admit(payload)
                self.assertEqual(rejected.exception.code, "generation_budget_exhausted")
                self.assertEqual(proxy.report()["reserved_cost_usd"], "0.001000000001")
            with self.gateway(max_cost_usd="0.001223703705", **rates) as proxy:
                self.assertEqual(proxy._admit(payload)[0], 200)
                self.assertEqual(proxy.report()["reserved_cost_usd"], "0.00122370370467036")
            self.assertEqual((context.prec, context.rounding), (4, ROUND_DOWN))

    def test_count_before_generation_and_injected_caps(self):
        with self.gateway() as proxy:
            status, _ = self.request(proxy, {"model": "fixed-model", "input": "repair", "max_output_tokens": 99999, "store": True})
            self.assertEqual(status, 200)
            report = proxy.report()
        self.assertEqual([r[0] for r in self.upstream.requests], ["/v1/responses/input_tokens", "/v1/responses"])
        count, generation = [item[2] for item in self.upstream.requests]
        self.assertEqual(count, {"model": "fixed-model", "input": "repair", "truncation": "disabled"})
        self.assertEqual(generation["max_output_tokens"], 20)
        self.assertEqual(generation["service_tier"], "default")
        self.assertFalse(generation["store"])
        self.assertEqual(generation["include"], ["reasoning.encrypted_content"])
        self.assertTrue(all(item[1]["Authorization"] == "Bearer " + KEY for item in self.upstream.requests))
        self.assertEqual((report["reserved_tokens"], report["reserved_cost_usd"]), (30, "0.00122"))
        self.assertEqual((report["reported_input_tokens"], report["reported_output_tokens"]), (10, 7))
        self.assertTrue(report["usage_complete"])
        self.assertEqual(report["transport_mode"], "injected")
        self.assertNotIn(KEY, json.dumps(report))
        self.assertNotIn(NONCE, json.dumps(report))
        self.assertNotIn("repair", json.dumps(report))

    def test_unapproved_routes_auth_fields_media_and_hosted_tools_never_reach_provider(self):
        with self.gateway() as proxy:
            self.assertEqual(self.request(proxy, auth="wrong")[0], 401)
            self.assertEqual(self.request(proxy, route="/v1/responses/input_tokens")[0], 404)
            self.assertEqual(self.request(proxy, route="/v1/responses?query=yes")[0], 404)
            self.assertEqual(self.request(proxy, method="GET")[0], 501)
            variants = [{"model": "other"}, {"tools": [{"type": "web_search_preview"}]},
                        {"tools": [{"type": "mcp", "server_url": "https://other"}]},
                        {"tools": [{"type": "code_interpreter"}]}, {"service_tier": "priority"},
                        {"truncation": "auto"}, {"previous_response_id": "resp_other"}, {"conversation": "conv_other"},
                        {"background": True}, {"prompt_cache_retention": "24h"}, {"stream": "true"},
                        {"input": [{"type": "item_reference", "id": "remote_file"}]},
                        {"input": [{"role": "user", "content": [{"type": "input_image", "image_url": "https://x"}]}]},
                        {"input": [{"role": "user", "content": [{"type": "input_file", "file_id": "file_x"}]}]},
                        {"tool_choice": {"type": "web_search"}}, {"text": {"format": {"type": "external_schema"}}},
                        {"reasoning": {"unknown": True}}]
            for variant in variants:
                payload = {"model": "fixed-model", "input": "repair", **variant}
                with self.subTest(variant=variant):
                    self.assertEqual(self.request(proxy, payload)[0], 400)
            for raw in (b'{"model":"fixed-model","model":"other","input":"x"}', b'{"model":"fixed-model","input":"x","top_p":NaN}'):
                self.assertEqual(self.request(proxy, raw=raw)[0], 400)
            self.assertEqual(proxy.report()["requests"], 0)
        self.assertEqual(self.upstream.requests, [])

    def test_function_tools_replayed_messages_reasoning_and_phase_are_supported(self):
        payload = {"model": "fixed-model", "input": [
            {"role": "user", "content": [{"type": "input_text", "text": "repair"}]},
            {"type": "message", "role": "assistant", "phase": "commentary", "content": [{"type": "output_text", "text": "checking", "annotations": []}]},
            {"type": "reasoning", "id": "rs_x", "summary": [{"type": "summary_text", "text": "reason"}], "encrypted_content": "opaque"},
            {"type": "function_call", "call_id": "call_x", "name": "read_file", "arguments": "{}"},
            {"type": "function_call_output", "call_id": "call_x", "output": "source"}],
            "tools": [{"type": "function", "name": "read_file", "parameters": {"type": "object", "properties": {"tools": {"type": "string"}}}}],
            "tool_choice": {"type": "function", "name": "read_file"}, "reasoning": {"effort": "high"},
            "parallel_tool_calls": False, "text": {"format": {"type": "text"}}}
        with self.gateway() as proxy:
            self.assertEqual(self.request(proxy, payload)[0], 200)
        count, generation = [item[2] for item in self.upstream.requests]
        self.assertEqual(count["input"], generation["input"])
        self.assertEqual(count["tools"], generation["tools"])

    def test_sdk_retries_each_pay_and_reserve_again_without_refund(self):
        self.upstream.generation_status = 429
        self.upstream.generation_body = json.dumps({"error": "secret: " + KEY}).encode()
        with self.gateway(total_tokens=60) as proxy:
            for _ in range(2):
                status, body = self.request(proxy)
                self.assertEqual(status, 502)
                self.assertNotIn(KEY.encode(), body)
            self.assertEqual(self.request(proxy)[0], 402)
            report = proxy.report()
        self.assertEqual((report["requests"], report["reserved_tokens"]), (2, 60))
        self.assertEqual(Decimal(report["reserved_cost_usd"]), Decimal("0.00244"))
        self.assertIsNone(report["reported_input_tokens"])
        self.assertFalse(report["usage_complete"])
        self.assertEqual(len(self.upstream.requests), 4)

    def test_input_token_and_dollar_denial_retain_only_count_fee(self):
        for kwargs in ({"total_tokens": 25}, {"max_cost_usd": "0.0011"}):
            self.upstream.requests.clear()
            with self.subTest(kwargs=kwargs), self.gateway(**kwargs) as proxy:
                self.assertEqual(self.request(proxy)[0], 402)
                report = proxy.report()
            self.assertEqual(report["reserved_tokens"], 0)
            self.assertEqual(report["reserved_cost_usd"], "0.001")
            self.assertEqual([r[0] for r in self.upstream.requests], ["/v1/responses/input_tokens"])

    def test_count_fee_is_reserved_before_any_network_and_request_cap_bounds_failures(self):
        with self.gateway(max_cost_usd="0.0001") as proxy:
            self.assertEqual(self.request(proxy)[0], 402)
            self.assertEqual(proxy.report()["requests"], 0)
        self.assertEqual(self.upstream.requests, [])
        self.upstream.count_status = 500
        with self.gateway(max_requests=2) as proxy:
            self.assertEqual(self.request(proxy)[0], 502)
            self.assertEqual(self.request(proxy)[0], 502)
            self.assertEqual(self.request(proxy)[0], 402)
            self.assertEqual(proxy.report()["reserved_cost_usd"], "0.002")
        self.assertEqual(len(self.upstream.requests), 2)

    def test_invalid_or_failed_count_never_admits_generation(self):
        for count in (b'{}', b'{"input_tokens":true}', b'{"input_tokens":-1}', b'{"input_tokens":1.5}',
                      b'{"input_tokens":10,"input_tokens":0}', b'{"input_tokens":1000000001}', b'not JSON', b'x' * 70000):
            self.upstream.count_body = count
            self.upstream.requests.clear()
            with self.subTest(count=count[:50]), self.gateway() as proxy:
                self.assertEqual(self.request(proxy)[0], 502)
                self.assertEqual(proxy.report()["reserved_cost_usd"], "0.001")
                self.assertEqual(proxy.report()["reserved_tokens"], 0)
            self.assertEqual(len(self.upstream.requests), 1)

    def test_redirects_are_not_followed(self):
        self.upstream.count_status = 307
        with self.gateway() as proxy:
            self.assertEqual(self.request(proxy)[0], 502)
        self.assertEqual(len(self.upstream.requests), 1)
        self.upstream.count_status, self.upstream.generation_status = 200, 302
        with self.gateway() as proxy:
            self.assertEqual(self.request(proxy)[0], 502)
            self.assertEqual(proxy.report()["reserved_tokens"], 30)
        self.assertEqual(len(self.upstream.requests), 3)

    def test_sse_completed_incomplete_failed_and_cancelled_accounting(self):
        for status in ("completed", "incomplete", "failed", "cancelled"):
            response = completed(status=status)
            self.upstream.generation_body = as_sse(response) if status != "cancelled" else json.dumps(response).encode()
            self.upstream.content_type = "text/event-stream" if status != "cancelled" else "application/json"
            with self.subTest(status=status), self.gateway() as proxy:
                self.assertEqual(self.request(proxy)[0], 200)
                report = proxy.report()
            self.assertEqual(report["reserved_tokens"], 30)
            self.assertEqual(report["requests_detail"][0]["status"], status)

    def test_missing_usage_is_null_and_truncated_or_nonunique_streams_are_unknown(self):
        response = completed()
        response.pop("usage")
        self.upstream.generation_body = json.dumps(response).encode()
        with self.gateway() as proxy:
            self.assertEqual(self.request(proxy)[0], 200)
            self.assertIsNone(proxy.report()["reported_output_tokens"])
        for body in (b'data: {"type":"response.created"}\n\n', as_sse(completed()) * 2,
                     b'data: {"type":"response.completed",\n\n', b'data: [DONE]\n\n'):
            self.upstream.generation_body, self.upstream.content_type = body, "text/event-stream"
            with self.subTest(body=body[:50]), self.gateway() as proxy:
                self.assertEqual(self.request(proxy)[0], 502)
                report = proxy.report()
            self.assertEqual(report["reserved_tokens"], 30)
            self.assertIsNone(report["reported_output_tokens"])
            self.assertEqual(report["requests_detail"][0]["status"], "generation_unknown")

    def test_truncated_http_body_is_unknown_even_with_complete_json(self):
        self.upstream.truncate = True
        with self.gateway() as proxy:
            self.assertEqual(self.request(proxy)[0], 502)
            report = proxy.report()
        self.assertEqual(report["reserved_tokens"], 30)
        self.assertEqual(report["requests_detail"][0]["status"], "generation_unknown")
        self.assertIsNone(report["reported_input_tokens"])

    def test_provider_exceeding_count_or_output_limit_closes_admission(self):
        for response in (completed(input_tokens=11), completed(output_tokens=21)):
            self.upstream.generation_body = json.dumps(response).encode()
            with self.subTest(response=response), self.gateway() as proxy:
                self.assertEqual(self.request(proxy)[0], 502)
                self.assertEqual(self.request(proxy)[0], 409)
                self.assertFalse(proxy.report()["reservation_integrity"])
                self.assertEqual(proxy.report()["requests"], 1)

    def test_oversized_request_and_response_are_bounded(self):
        with self.gateway(max_request_bytes=20) as proxy:
            self.assertEqual(self.request(proxy)[0], 413)
        self.assertEqual(self.upstream.requests, [])
        raw = json.dumps({"model": "fixed-model", "input": "é" * 100}, ensure_ascii=False).encode()
        self.assertLess(len(raw), 500)
        with self.gateway(max_request_bytes=500) as proxy:
            self.assertEqual(self.request(proxy, raw=raw)[0], 413)
            self.assertEqual(proxy.report()["requests"], 0)
        self.assertEqual(self.upstream.requests, [])
        with self.gateway(max_response_bytes=20) as proxy:
            self.assertEqual(self.request(proxy)[0], 502)
            self.assertEqual(proxy.report()["reserved_tokens"], 30)
            self.assertEqual(proxy.report()["requests_detail"][0]["status"], "generation_unknown")

    def test_client_disconnect_preserves_generation_reservation(self):
        self.upstream.delay = 0.1
        with self.gateway() as proxy:
            conn = http.client.HTTPConnection(*proxy.address, timeout=2)
            conn.request("POST", "/v1/responses", json.dumps({"model": "fixed-model", "input": "x"}),
                         {"Authorization": "Bearer " + NONCE, "Content-Type": "application/json"})
            conn.close()
            deadline = time.monotonic() + 2
            while len(self.upstream.requests) < 2 and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertEqual(proxy.report()["reserved_tokens"], 30)
        self.assertTrue(proxy.report()["closed"])
        with self.assertRaises(RuntimeError):
            proxy.__enter__()

    def test_concurrent_requests_cannot_double_spend(self):
        with self.gateway(total_tokens=30) as proxy:
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                results = list(pool.map(lambda _: self.request(proxy)[0], range(2)))
            self.assertEqual(sorted(results), [200, 402])
            self.assertEqual(proxy.report()["reserved_tokens"], 30)
        self.assertEqual(len(self.upstream.requests), 2)

    def test_success_body_redacts_echoed_credentials(self):
        response = completed()
        response["output"] = [{"text": KEY + " " + NONCE}]
        self.upstream.generation_body = json.dumps(response).encode()
        with self.gateway() as proxy:
            status, body = self.request(proxy)
            self.assertEqual(status, 200)
            self.assertNotIn(KEY.encode(), body)
            self.assertNotIn(NONCE.encode(), body)
            self.assertNotIn(KEY, json.dumps(proxy.report()))

    def test_total_upstream_deadline_stops_drip_without_refund(self):
        self.upstream.drip = True
        started = time.monotonic()
        with self.gateway(timeout_seconds=0.15) as proxy:
            try:
                status, _ = self.request(proxy)
                self.assertEqual(status, 502)
            except (OSError, http.client.HTTPException):
                pass  # Client deadline may close before the gateway error is sent.
        self.assertLess(time.monotonic() - started, 1.0)
        self.assertEqual(proxy.report()["reserved_tokens"], 30)
        self.assertEqual(proxy.report()["requests_detail"][0]["status"], "generation_unknown")

    def test_close_interrupts_incomplete_client_body(self):
        proxy = self.gateway(timeout_seconds=2)
        proxy.__enter__()
        client = socket.create_connection(proxy.address, timeout=1)
        client.sendall(("POST /v1/responses HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer " + NONCE
                        + "\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{").encode())
        time.sleep(0.05)
        started = time.monotonic()
        proxy.__exit__()
        client.close()
        self.assertLess(time.monotonic() - started, 0.5)
        self.assertEqual(self.upstream.requests, [])


if __name__ == "__main__":
    unittest.main()
