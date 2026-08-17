#!/usr/bin/env python3
"""Sydney OfficeAgent probe — send requests with different hub/tone configs
and dump all WS frames to see how the server responds.

Tests:
1. Normal ChatHub tone (claude) — baseline
2. OfficeAgent-style payload with agent fields
3. Different WS paths if ChatHub rejects agent payload
"""

from __future__ import annotations

import asyncio
import json
import uuid
import urllib.parse
import time
from pathlib import Path

import websockets

TOKEN_FILE = "~/.config/m365-copilot2api/accounts.json"
PROXY = "http://127.0.0.1:7897"
RS = "\x1e"

VARIANTS = ",".join([
    "EnableMcpServerWidgets",
    "feature.EnableMcpServerWidgets",
    "feature.EnableLuForChatCIQ",
    "feature.enableChatCIQPlugin",
    "EnableRequestPlugins",
    "feature.EnableSensitivityLabels",
    "feature.IsCustomEngineCopilotEnabled",
    "feature.bizchatfluxv3",
    "feature.enablechatpages",
    "feature.turnOnWorkTabRecommendation",
    "feature.IsStreamingModeInChatRequestEnabled",
    "IncludeSourceAttributionsConcise",
    "SkipPublishEmptyMessage",
    "Enable3PActionProgressMessages",
    "feature.enableCitationsForSynthesisData",
    "feature.enableGenerateGraphicArtOptionsSet",
    "cdximagen",
    "feature.OfficeWebToHelix",
    "feature.M365TeamsHubToHelix",
    "feature.OwaHubToHelix",
    "Agt_bizchat_enableGpt5ForHelix",
])

AGENT_VARIANTS = VARIANTS + ",feature.EnableDiceberry,feature.EnableDiceberryCoTComponent,feature.EnableSydneyBerry"


def load_token(path: str) -> dict:
    data = json.loads(Path(path).expanduser().read_text())
    return data["accounts"][0]


def build_ws_url(acc: dict, session_id: str, conversation_id: str, request_id: str,
                 path_suffix: str = "Chathub", scenario: str = "OfficeWebIncludedCopilot",
                 variants: str = VARIANTS) -> str:
    params = {
        "chatsessionid": request_id,
        "clientrequestid": request_id,
        "X-SessionId": session_id,
        "ConversationId": conversation_id,
        "access_token": acc["accessToken"],
        "variants": variants,
        "source": '"officeweb"',
        "product": "Office",
        "agentHost": "Bizchat.FullScreen",
        "licenseType": "Starter",
        "agent": "web",
        "scenario": scenario,
    }
    qs = urllib.parse.urlencode(params, safe='",')
    return f"wss://substrate.office.com/m365Copilot/{path_suffix}/{acc['oid']}@{acc['tid']}?{qs}"


def base_message(text: str, request_id: str) -> dict:
    return {
        "author": "user",
        "inputMethod": "Keyboard",
        "text": text,
        "requestId": request_id,
        "locationInfo": {"timeZoneOffset": 8, "timeZone": "Asia/Shanghai"},
        "locale": "en-US",
        "messageType": "Chat",
        "experienceType": "Default",
        "adaptiveCards": [],
        "clientPreferences": {},
        "entityAnnotationTypes": ["People", "File", "Event", "Email", "TeamsMessage"],
    }


def normal_chat_payload(text, session_id, conversation_id, request_id, tone) -> str:
    message = base_message(text, request_id)
    chat = {
        "arguments": [{
            "source": "officeweb",
            "clientCorrelationId": str(uuid.uuid4()),
            "sessionId": session_id,
            "optionsSets": [
                "search_result_progress_messages_with_search_queries",
                "update_textdoc_response_after_streaming",
                "deepleo_networking_timeout_10minutes_canmore",
                "cwc_flux_v3",
                "flux_v3_progress_messages",
                "enable_batch_token_processing",
                "enable_gg_gpt",
            ],
            "options": {},
            "allowedMessageTypes": [
                "Chat", "Suggestion", "Disengaged", "Progress",
                "EndOfRequest", "InternalLoaderMessage",
            ],
            "sliceIds": [],
            "threadLevelGptId": {},
            "conversationId": conversation_id,
            "traceId": str(uuid.uuid4()),
            "isStartOfSession": True,
            "productThreadType": "Office",
            "clientInfo": {
                "clientPlatform": "mcmcopilot-web",
                "clientAppName": "Office",
            },
            "tone": tone,
            "streamingMode": "ConciseWithPadding",
            "message": message,
            "plugins": [],
        }],
        "invocationId": "0",
        "target": "chat",
        "type": 4,
    }
    metrics = {
        "arguments": [{"Timestamps": {
            "ConnectionStart": "", "UserInputStart": "",
            "ConnectionEstablished": "", "UserInputSubmit": "",
        }}],
        "target": "Metrics",
        "type": 1,
    }
    return json.dumps(chat, separators=(",", ":")) + RS + json.dumps(metrics, separators=(",", ":")) + RS


def agent_chat_payload(text, session_id, conversation_id, request_id,
                        tone="claude", agent_id="copilot_with_sydney_berry") -> str:
    """OfficeAgent-style payload — adds agent-specific fields."""
    message = base_message(text, request_id)
    chat = {
        "arguments": [{
            "source": "officeweb",
            "clientCorrelationId": str(uuid.uuid4()),
            "sessionId": session_id,
            "optionsSets": [
                "search_result_progress_messages_with_search_queries",
                "update_textdoc_response_after_streaming",
                "deepleo_networking_timeout_10minutes_canmore",
                "cwc_flux_v3",
                "flux_v3_progress_messages",
                "enable_batch_token_processing",
                "enable_gg_gpt",
                "copilot_with_sydney_berry",
            ],
            "options": {},
            "allowedMessageTypes": [
                "Chat", "Suggestion", "Disengaged", "Progress",
                "EndOfRequest", "InternalLoaderMessage",
                "ActionCardRequest", "GenerateCode", "SearchQuery",
            ],
            "sliceIds": [],
            "threadLevelGptId": {"id": agent_id},
            "conversationId": conversation_id,
            "traceId": str(uuid.uuid4()),
            "isStartOfSession": True,
            "productThreadType": "Office",
            "clientInfo": {
                "clientPlatform": "mcmcopilot-web",
                "clientAppName": "Office",
            },
            "tone": tone,
            "streamingMode": "ConciseWithPadding",
            "message": message,
            "plugins": [],
            "sydneyConfigurationParameters": {
                "variants": ["copilot_with_sydney_berry"],
            },
        }],
        "invocationId": "0",
        "target": "chat",
        "type": 4,
    }
    metrics = {
        "arguments": [{"Timestamps": {
            "ConnectionStart": "", "UserInputStart": "",
            "ConnectionEstablished": "", "UserInputSubmit": "",
        }}],
        "target": "Metrics",
        "type": 1,
    }
    return json.dumps(chat, separators=(",", ":")) + RS + json.dumps(metrics, separators=(",", ":")) + RS


async def probe(acc: dict, label: str, ws_url: str, payload: str, max_frames: int = 30) -> None:
    print(f"\n{'='*60}")
    print(f"PROBE: {label}")
    print(f"WS URL prefix: {ws_url[:80]}...")
    print(f"{'='*60}")

    headers = {
        "Origin": "https://m365.cloud.microsoft",
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0",
    }

    try:
        async with websockets.connect(
            ws_url, additional_headers=headers,
            open_timeout=15, close_timeout=5, max_size=8*1024*1024,
            proxy=PROXY,
        ) as ws:
            await ws.send('{"protocol":"json","version":1}' + RS)
            hs = await asyncio.wait_for(ws.recv(), timeout=10)
            print(f"  handshake: {repr(hs)[:120]}")

            await ws.send(payload)
            print(f"  payload sent ({len(payload)} bytes)")

            full_text = []
            for i in range(max_frames):
                try:
                    msg = await asyncio.wait_for(ws.recv(), timeout=25)
                except asyncio.TimeoutError:
                    print(f"  frame {i}: TIMEOUT")
                    break

                if isinstance(msg, bytes):
                    msg = msg.decode("utf-8", "replace")

                for part in [p for p in msg.split(RS) if p]:
                    try:
                        obj = json.loads(part)
                    except json.JSONDecodeError:
                        print(f"  frame {i} raw: {part[:200]}")
                        continue

                    t = obj.get("type")
                    target = obj.get("target")

                    if t == 6:
                        await ws.send(json.dumps({"type": 6}) + RS)
                        continue

                    if t == 1 and target == "update":
                        for arg in obj.get("arguments") or []:
                            if not isinstance(arg, dict):
                                continue
                            if "writeAtCursor" in arg:
                                full_text.append(arg["writeAtCursor"])
                            for m in arg.get("messages") or []:
                                if isinstance(m, dict) and m.get("author") == "bot":
                                    if m.get("text"):
                                        full_text.append(m["text"])
                                    sig = m.get("conversationSignature")
                                    if sig:
                                        print(f"  conversationSignature: {sig[:40]}...")
                            thr = arg.get("throttling")
                            if thr:
                                print(f"  throttling: {thr}")
                    elif t == 2:
                        item = obj.get("item") or {}
                        res = item.get("result") or {}
                        if res.get("message"):
                            print(f"  result message: {str(res['message'])[:200]}")
                        sig = item.get("conversationSignature")
                        if sig:
                            print(f"  RESULT conversationSignature: {sig[:40]}...")
                        cid = item.get("conversationId")
                        if cid:
                            print(f"  RESULT conversationId: {cid}")
                        print(f"  result value: {res.get('value')}")
                    elif t == 3:
                        err = obj.get("error")
                        if err:
                            print(f"  COMPLETION ERROR: {json.dumps(err, indent=2)[:500]}")
                    elif t == 7:
                        print(f"  CLOSE: {json.dumps(obj, indent=2)[:300]}")

                    if t in (2, 3, 7):
                        answer = "".join(full_text)
                        print(f"  ANSWER ({len(answer)} chars): {answer[:300]}")
                        return

            answer = "".join(full_text)
            print(f"  ANSWER (partial, {len(answer)} chars): {answer[:300]}")

    except websockets.exceptions.InvalidHandshake as e:
        print(f"  HANDSHAKE FAILED: {e}")
    except websockets.exceptions.ConnectionClosed as e:
        print(f"  CONNECTION CLOSED: code={e.code} reason={e.reason[:200] if e.reason else ''}")
    except Exception as e:
        print(f"  ERROR: {type(e).__name__}: {e}")


async def main() -> None:
    acc = load_token(TOKEN_FILE)
    text = "Say only: pong"

    session_id = str(uuid.uuid4())
    conversation_id = str(uuid.uuid4())
    request_id = str(uuid.uuid4())

    # Test 1: Normal ChatHub with claude tone
    url1 = build_ws_url(acc, session_id, conversation_id, request_id)
    p1 = normal_chat_payload(text, session_id, conversation_id, request_id, "claude")
    await probe(acc, "ChatHub + tone=claude", url1, p1)

    await asyncio.sleep(2)

    # Test 2: ChatHub with agent payload (sydney_berry fields)
    s2 = str(uuid.uuid4())
    c2 = str(uuid.uuid4())
    r2 = str(uuid.uuid4())
    url2 = build_ws_url(acc, s2, c2, r2, variants=AGENT_VARIANTS)
    p2 = agent_chat_payload(text, s2, c2, r2, tone="claude", agent_id="copilot_with_sydney_berry")
    await probe(acc, "ChatHub + agent payload + sydney_berry", url2, p2)

    await asyncio.sleep(2)

    # Test 3: Try OfficeAgent WS path (if different from Chathub)
    s3 = str(uuid.uuid4())
    c3 = str(uuid.uuid4())
    r3 = str(uuid.uuid4())
    url3 = build_ws_url(acc, s3, c3, r3, path_suffix="OfficeAgent", scenario="OfficeWebPaidCopilot", variants=AGENT_VARIANTS)
    p3 = agent_chat_payload(text, s3, c3, r3, tone="claude", agent_id="copilot_with_sydney_berry")
    await probe(acc, "OfficeAgent path + agent payload", url3, p3)

    await asyncio.sleep(2)

    # Test 4: ChatHub + tone=auto (magic) as baseline
    s4 = str(uuid.uuid4())
    c4 = str(uuid.uuid4())
    r4 = str(uuid.uuid4())
    url4 = build_ws_url(acc, s4, c4, r4)
    p4 = normal_chat_payload(text, s4, c4, r4, "auto")
    await probe(acc, "ChatHub + tone=auto (magic baseline)", url4, p4)


if __name__ == "__main__":
    asyncio.run(main())
