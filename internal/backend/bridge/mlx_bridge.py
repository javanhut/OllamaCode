# /// script
# requires-python = ">=3.11,<3.14"
# dependencies = [
#   "mlx-lm>=0.21.0",
#   "mlx-vlm>=0.1.12",
#   "fastapi>=0.110",
#   "uvicorn>=0.29",
#   "huggingface_hub>=0.23",
#   "pillow>=10.0",
# ]
# ///
"""OllamaCode MLX bridge.

A single OpenAI-compatible HTTP server that fronts BOTH mlx_lm (text models)
and mlx_vlm (vision models). OllamaCode launches and supervises this script via
`uv run`, which provisions the dependencies above into an isolated environment
on first run — the user never installs anything by hand.

Routing: each /v1/chat/completions request is dispatched to mlx_vlm when the
target model is a vision-language model (detected from its config, or because
the request carries image content) and to mlx_lm otherwise. One model is kept
resident at a time and reused across requests.

Endpoints:
  GET  /v1/models             list locally-cached models
  POST /v1/chat/completions   chat (streaming SSE or one-shot)
  POST /v1/embeddings         501 (not supported by the MLX bridge)
  GET  /health                liveness probe used by the supervisor
"""

import argparse
import base64
import io
import json
import time
import uuid
from typing import Any, Dict, List, Optional, Tuple

from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, StreamingResponse

app = FastAPI()

# ----- model registry / lazy loading -------------------------------------------------

# One resident model at a time: (model_id, kind, handle_a, handle_b)
#   text   -> handle_a=model, handle_b=tokenizer
#   vision -> handle_a=model, handle_b=processor, plus _VISION_CONFIG
_LOADED: Dict[str, Any] = {"id": None, "kind": None, "a": None, "b": None, "config": None}

_VLM_HINTS = (
    "vl", "vision", "llava", "qwen2_vl", "qwen2_5_vl", "qwen3_vl", "idefics",
    "pixtral", "gemma3", "smolvlm", "internvl", "molmo", "paligemma", "florence",
)


def _read_model_config(model_id: str) -> Dict[str, Any]:
    """Fetch a model's config.json (from the HF cache or hub)."""
    try:
        from huggingface_hub import hf_hub_download

        path = hf_hub_download(model_id, "config.json")
        with open(path, "r") as fh:
            return json.load(fh)
    except Exception:
        return {}


def is_vision_model(model_id: str) -> bool:
    cfg = _read_model_config(model_id)
    if "vision_config" in cfg or "image_token_index" in cfg:
        return True
    mt = str(cfg.get("model_type", "")).lower()
    arch = " ".join(cfg.get("architectures", [])).lower()
    blob = mt + " " + arch + " " + model_id.lower()
    return any(h in blob for h in _VLM_HINTS)


def _messages_have_images(messages: List[Dict[str, Any]]) -> bool:
    for m in messages:
        content = m.get("content")
        if isinstance(content, list):
            for part in content:
                if isinstance(part, dict) and part.get("type") in ("image_url", "image"):
                    return True
    return False


def load_text(model_id: str):
    from mlx_lm import load

    if _LOADED["id"] == model_id and _LOADED["kind"] == "text":
        return _LOADED["a"], _LOADED["b"]
    model, tokenizer = load(model_id)
    _LOADED.update(id=model_id, kind="text", a=model, b=tokenizer, config=None)
    return model, tokenizer


def load_vision(model_id: str):
    from mlx_vlm import load
    from mlx_vlm.utils import load_config

    if _LOADED["id"] == model_id and _LOADED["kind"] == "vision":
        return _LOADED["a"], _LOADED["b"], _LOADED["config"]
    model, processor = load(model_id)
    config = load_config(model_id)
    _LOADED.update(id=model_id, kind="vision", a=model, b=processor, config=config)
    return model, processor, config


# ----- message / image helpers -------------------------------------------------------

def _flatten_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for part in content:
            if isinstance(part, dict) and part.get("type") == "text":
                parts.append(part.get("text", ""))
        return "".join(parts)
    return ""


def _normalize_text_messages(messages: List[Dict[str, Any]]) -> List[Dict[str, str]]:
    out = []
    for m in messages:
        out.append({"role": m.get("role", "user"), "content": _flatten_text(m.get("content"))})
    return out


def _extract_images(messages: List[Dict[str, Any]]):
    """Return PIL images referenced via OpenAI image_url parts (data URIs only)."""
    from PIL import Image

    images = []
    for m in messages:
        content = m.get("content")
        if not isinstance(content, list):
            continue
        for part in content:
            if not isinstance(part, dict):
                continue
            if part.get("type") == "image_url":
                url = (part.get("image_url") or {}).get("url", "")
                if url.startswith("data:"):
                    b64 = url.split(",", 1)[-1]
                    images.append(Image.open(io.BytesIO(base64.b64decode(b64))).convert("RGB"))
    return images


def _gen_params(body: Dict[str, Any]) -> Dict[str, Any]:
    params: Dict[str, Any] = {}
    if body.get("max_tokens"):
        params["max_tokens"] = int(body["max_tokens"])
    else:
        params["max_tokens"] = 2048
    if body.get("temperature") is not None:
        params["temp"] = float(body["temperature"])
    if body.get("top_p") is not None:
        params["top_p"] = float(body["top_p"])
    return params


# ----- prompt assembly ---------------------------------------------------------------

def _text_prompt(tokenizer, messages, tools):
    msgs = _normalize_text_messages(messages)
    kwargs = dict(add_generation_prompt=True, tokenize=False)
    if tools:
        try:
            return tokenizer.apply_chat_template(msgs, tools=tools, **kwargs)
        except Exception:
            pass
    try:
        return tokenizer.apply_chat_template(msgs, **kwargs)
    except Exception:
        # Last resort: concatenate.
        return "\n".join(f"{m['role']}: {m['content']}" for m in msgs) + "\nassistant:"


# ----- OpenAI envelope helpers -------------------------------------------------------

def _chat_id() -> str:
    return "chatcmpl-" + uuid.uuid4().hex


def _now() -> int:
    return int(time.time())


def _chunk(cid, model, delta, finish=None):
    return {
        "id": cid,
        "object": "chat.completion.chunk",
        "created": _now(),
        "model": model,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }


# ----- generators --------------------------------------------------------------------

def _stream_text(model_id, messages, tools, params):
    from mlx_lm import stream_generate

    model, tokenizer = load_text(model_id)
    prompt = _text_prompt(tokenizer, messages, tools)
    cid = _chat_id()
    yield _chunk(cid, model_id, {"role": "assistant", "content": ""})
    prompt_tokens, completion_tokens = 0, 0
    sg_kwargs = {k: v for k, v in params.items() if k in ("max_tokens", "temp", "top_p")}
    for resp in stream_generate(model, tokenizer, prompt, **sg_kwargs):
        text = getattr(resp, "text", "")
        completion_tokens += 1
        prompt_tokens = getattr(resp, "prompt_tokens", prompt_tokens)
        if text:
            yield _chunk(cid, model_id, {"content": text})
    final = _chunk(cid, model_id, {}, finish="stop")
    final["usage"] = {"prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens}
    yield final


def _stream_vision(model_id, messages, params):
    from mlx_vlm import generate as vlm_generate
    from mlx_vlm.prompt_utils import apply_chat_template as vlm_template

    model, processor, config = load_vision(model_id)
    images = _extract_images(messages)
    msgs = _normalize_text_messages(messages)
    prompt = vlm_template(processor, config, msgs, num_images=len(images))
    cid = _chat_id()
    yield _chunk(cid, model_id, {"role": "assistant", "content": ""})

    gkwargs = {"max_tokens": params.get("max_tokens", 2048)}
    if "temp" in params:
        gkwargs["temperature"] = params["temp"]
    completion_tokens = 0
    try:
        # Newer mlx_vlm supports streaming via a generator.
        for out in vlm_generate(model, processor, prompt, images, stream=True, **gkwargs):
            text = out if isinstance(out, str) else getattr(out, "text", "")
            if text:
                completion_tokens += 1
                yield _chunk(cid, model_id, {"content": text})
    except TypeError:
        # Older mlx_vlm: one-shot string.
        out = vlm_generate(model, processor, prompt, images, **gkwargs)
        text = out if isinstance(out, str) else getattr(out, "text", str(out))
        yield _chunk(cid, model_id, {"content": text})
    final = _chunk(cid, model_id, {}, finish="stop")
    final["usage"] = {"prompt_tokens": 0, "completion_tokens": completion_tokens}
    yield final


def _route_stream(model_id, messages, tools, params):
    if _messages_have_images(messages) or is_vision_model(model_id):
        yield from _stream_vision(model_id, messages, params)
    else:
        yield from _stream_text(model_id, messages, tools, params)


# ----- endpoints ---------------------------------------------------------------------

@app.get("/health")
def health():
    return {"status": "ok"}


@app.get("/v1/models")
def list_models():
    data = []
    try:
        from huggingface_hub import scan_cache_dir

        seen = set()
        for repo in scan_cache_dir().repos:
            if repo.repo_type != "model":
                continue
            rid = repo.repo_id
            if rid in seen:
                continue
            seen.add(rid)
            data.append({"id": rid, "object": "model", "owned_by": "mlx"})
    except Exception:
        pass
    if _LOADED["id"] and _LOADED["id"] not in {d["id"] for d in data}:
        data.append({"id": _LOADED["id"], "object": "model", "owned_by": "mlx"})
    return {"object": "list", "data": data}


@app.post("/v1/embeddings")
def embeddings():
    return JSONResponse(
        status_code=501,
        content={"error": {"message": "embeddings are not supported by the MLX bridge", "type": "not_implemented"}},
    )


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    body = await request.json()
    model_id = body.get("model")
    if not model_id:
        return JSONResponse(status_code=400, content={"error": {"message": "missing 'model'"}})
    messages = body.get("messages", [])
    tools = body.get("tools")
    params = _gen_params(body)
    stream = bool(body.get("stream"))

    if stream:
        def sse():
            try:
                for chunk in _route_stream(model_id, messages, tools, params):
                    yield "data: " + json.dumps(chunk) + "\n\n"
            except Exception as exc:  # surface load/generation errors to the client
                err = _chunk(_chat_id(), model_id, {"content": f"\n[bridge error] {exc}"}, finish="stop")
                yield "data: " + json.dumps(err) + "\n\n"
            yield "data: [DONE]\n\n"

        return StreamingResponse(sse(), media_type="text/event-stream")

    # Non-streaming: drain the same generator and assemble one message.
    try:
        content_parts: List[str] = []
        usage = {"prompt_tokens": 0, "completion_tokens": 0}
        for chunk in _route_stream(model_id, messages, tools, params):
            choice = chunk["choices"][0]
            delta = choice.get("delta", {})
            if delta.get("content"):
                content_parts.append(delta["content"])
            if "usage" in chunk:
                usage = chunk["usage"]
    except Exception as exc:
        return JSONResponse(status_code=500, content={"error": {"message": str(exc)}})

    return {
        "id": _chat_id(),
        "object": "chat.completion",
        "created": _now(),
        "model": model_id,
        "choices": [
            {
                "index": 0,
                "message": {"role": "assistant", "content": "".join(content_parts)},
                "finish_reason": "stop",
            }
        ],
        "usage": usage,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=11550)
    args = parser.parse_args()

    import uvicorn

    uvicorn.run(app, host=args.host, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
