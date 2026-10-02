"""Images on agent input: workload.attachments with the first prompt and
`attachments` on POST /input (lux steer --image), through each adapter to
lux-fake, which reports every image as it got it (its protocol's shape,
type, size, sha256, and for Codex the file it read). The images are also
files in $LUX_INPUTS, on the Run's state, so they survive a stop and a
resume on another host; records and events say what they were, never
their bytes."""

from __future__ import annotations

import base64
import hashlib
import json
import time

import pytest

from conftest import CLIError, fake_agent, generic, harnesses
from env import wait_until
from pngword import square_png, word_png

# The shape each adapter gives the agent an image in (lux-fake's name).
SHAPES = {"claude-code": "claude", "codex": "codex-local", "acp": "acp", "opencode": "opencode-file"}


def _seen(data: bytes, shape: str, typ: str = "image/png") -> str:
    """lux-fake's reply line for an image it got."""
    typ = "file" if shape == "codex-local" else typ
    return f"image {shape} {typ} {len(data)} sha256={hashlib.sha256(data).hexdigest()}"


def _meta(name: str, data: bytes) -> dict:
    return {"name": name, "contentType": "image/png", "size": len(data), "sha256": hashlib.sha256(data).hexdigest()}


def _inputs_file(host, run_id: str, rel: str) -> str:
    """sha256 of a file under the home volume's $LUX_INPUTS, read on the
    host ("" if it is not there)."""
    return host.exec("sh", "-c", f"f=$(podman volume inspect --format '{{{{.Mountpoint}}}}' lux-{run_id}-home)/.lux-inputs/{rel}; "
                     "[ -f \"$f\" ] && sha256sum \"$f\" | cut -d' ' -f1; true").strip()


@harnesses(lambda h: h.adapter in SHAPES)
def test_images_reach_the_agent_and_survive_a_move(lux, runners, hosts, harness, tmp_path):
    if harness.real:
        pytest.skip("test_real_agent_reads_an_image covers the real CLIs")
    a, b = hosts[0], hosts[1]
    runners.start(a)
    shape = SHAPES[harness.harness.adapter]
    prompt_img, steer_img, steer_img2 = square_png(10), square_png(120), square_png(240)
    spec = harness.spec("echo prompt-done", workload={
        "attachments": [{"name": "first.png", "contentType": "image/png", "data": base64.b64encode(prompt_img).decode()}]})
    run_id = lux.submit(spec)
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    out = lux.logs(run_id)
    # OpenCode's first prompt is an ACP session/prompt; its steers go
    # through prompt_async.
    prompt_shape = "acp" if shape == "opencode-file" else shape
    assert _seen(prompt_img, prompt_shape) in out and "prompt-done" in out, out
    if shape == "codex-local":
        assert "/home/agent/.lux-inputs/prompt/1-first.png" in out, out

    # A steer with two images, one with text, through the CLI, while a turn
    # runs (OpenCode takes it through prompt_async then).
    for name, data in (("a.png", steer_img), ("b b.png", steer_img2)):
        (tmp_path / name).write_bytes(data)
    lux.run("steer", run_id, "sleep 3\necho busy-done")
    lux.wait_activity(run_id, "busy", timeout=harness.timeout)
    lux.run("steer", run_id, "echo steer-done", "--image", str(tmp_path / "a.png"), "--image", str(tmp_path / "b b.png"),
            "--request-id", "img-1")
    wait_until(lambda: "steer-done" in lux.logs(run_id), harness.timeout, 0.3, "the steer never ran")
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    out = lux.logs(run_id)
    # One message: both images, in order, before the text ran.
    i1, i2, done = out.find(_seen(steer_img, shape)), out.find(_seen(steer_img2, shape)), out.find("steer-done")
    assert 0 <= i1 < i2 < done, out
    # An image alone, no text, to an idle agent (a turn of its own).
    lux.run("steer", run_id, "--image", str(tmp_path / "a.png"), "--request-id", "img-2")
    wait_until(lambda: _seen(steer_img, prompt_shape) in lux.logs(run_id).replace(_seen(steer_img, shape), "", 1),
               harness.timeout, 0.3, "image-only steer never seen")

    # Metadata in the records and events; no bytes anywhere.
    records = lux.records(run_id, "--events")
    acks = {r["event"]["data"]["requestId"]: r["event"]["data"] for r in records
            if r.get("event", {}).get("type") == "lux.input"}
    assert acks["prompt"]["attachments"] == [_meta("first.png", prompt_img)], acks["prompt"]
    assert acks["img-1"]["attachments"] == [_meta("a.png", steer_img), _meta("b b.png", steer_img2)], acks["img-1"]
    delivered = wait_until(lambda: {e["data"]["requestId"]: e["data"] for e in lux.events(run_id, "input.delivered")}.get("img-1"),
                           20, 0.3, "no input.delivered for img-1")
    assert delivered["attachments"] == acks["img-1"]["attachments"], delivered
    inputs = lux.events(run_id, "input")
    assert [e["data"].get("attachments") for e in inputs if e["data"]["requestId"] == "img-1"] == [acks["img-1"]["attachments"]]
    everything = lux.run("logs", run_id, "-o", "json", "--events").stdout + json.dumps(lux.json("events", run_id))
    for data in (prompt_img, steer_img, steer_img2):
        assert base64.b64encode(data).decode() not in everything, "an image's bytes are in a record or event"

    # $LUX_INPUTS has the bytes, and keeps them through a stop and a
    # resume on another host.
    want = {"prompt/1-first.png": prompt_img, "img-1/1-a.png": steer_img, "img-2/1-a.png": steer_img}
    for rel, data in want.items():
        assert _inputs_file(a, run_id, rel) == hashlib.sha256(data).hexdigest(), rel
    second = a.exec("sh", "-c", f"ls $(podman volume inspect --format '{{{{.Mountpoint}}}}' lux-{run_id}-home)/.lux-inputs/img-1").split()
    assert len(second) == 2 and second[0] == "1-a.png" and second[1].startswith("2-b_b.png-"), second
    lux.run("stop", run_id, "--wait")
    lux.wait_uploaded(run_id)
    runners.stop(a)
    runners.start(b)
    lux.run("resume", run_id, "--wait", *harness.resume_secrets(spec))
    for rel, data in want.items():
        assert _inputs_file(b, run_id, rel) == hashlib.sha256(data).hexdigest(), f"{rel} after the move"
    # The workload sees them there, as $LUX_INPUTS says.
    lux.run("steer", run_id, "print-secret LUX_INPUTS")
    wait_until(lambda: "/home/agent/.lux-inputs" in lux.logs(run_id), harness.timeout, 0.3, "LUX_INPUTS not set")
    # The first prompt's images are not given again on resume.
    assert lux.logs(run_id).count(_seen(prompt_img, prompt_shape)) == 1
    lux.run("cancel", run_id)


def test_real_agent_reads_an_image(lux, runners, hosts, harness, tmp_path):
    """The real CLI sees a steer's image as an image: asked what word is
    written in it, it answers the word (it is in no text it was given).
    Sent to the idle agent, and again mid-turn (OpenCode then takes it
    through prompt_async rather than ACP)."""
    if not harness.real:
        pytest.skip("the fake variants are covered above")
    runners.start(hosts[0])
    run_id = lux.submit(harness.spec("Reply with just: ready"))
    lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    for word, busy in (("MANGO", False), ("TULIP", True)):
        img = tmp_path / f"{word.lower()}.png"
        img.write_bytes(word_png(word))
        since = lux.records(run_id)[-1]["cursor"]
        if busy:
            lux.run("steer", run_id, "Run `sleep 15` with your shell tool, in the foreground, then reply: slept")
            lux.wait_activity(run_id, "busy", timeout=harness.timeout)
            time.sleep(3)
        rid = f"real-{word.lower()}"
        lux.run("steer", run_id, "What word is written in the image? Reply with just the word.",
                "--image", str(img), "--request-id", rid)
        try:
            reply = wait_until(lambda: (out := lux.logs(run_id, "--since", since)) and word in out.upper() and out,
                               harness.timeout, 1, f"the agent never named {word}")
        except AssertionError as e:
            seen = [json.dumps(r.get("event"))[:400] for r in lux.records(run_id, "--events") if r.get("event")]
            raise AssertionError(f"{e}; output: {lux.logs(run_id)[-1500:]!r}; events: {seen[-30:]}") from None
        ack = next(r["event"]["data"] for r in lux.records(run_id, "--events")
                   if r.get("event", {}).get("type") == "lux.input" and r["event"]["data"]["requestId"] == rid)
        print(f"{harness.id} {'mid-turn' if busy else 'idle'} lands={ack.get('lands')} receipt={ack.get('receipt')} replied: {reply.strip()!r}")
        assert ack["phase"] == "accepted" and ack["attachments"] == [_meta(img.name, word_png(word))], ack
        lux.wait_activity(run_id, "idle", timeout=harness.timeout)
    lux.run("cancel", run_id)


def test_images_refused(lux, runners, hosts, fake_image, tmp_path):
    """A generic Run takes no images; a bad one is refused before luxd
    queues anything, naming it."""
    runners.start(hosts[0])
    plain = lux.submit(generic(fake_image, "lux-fake", "plain"))
    lux.wait_state(plain, "running")
    img = tmp_path / "x.png"
    img.write_bytes(square_png(1))
    with pytest.raises(CLIError) as e:
        lux.run("steer", plain, "hi", "--image", str(img))
    assert "attachments_unsupported" in e.value.stderr or "nowhere to put an image" in e.value.stderr, e.value.stderr
    mismatched = [{"name": "x.png", "contentType": "image/gif", "data": base64.b64encode(square_png(1)).decode()}]
    with pytest.raises(CLIError) as e:
        lux.submit(fake_agent(fake_image, "echo hi", workload={"attachments": mismatched}))
    assert "does not match its bytes" in e.value.stderr, e.value.stderr
    lux.run("cancel", plain)


def test_acp_agent_without_images(lux, runners, hosts, fake_image, tmp_path):
    """An ACP agent that did not advertise images: the input is accepted
    by luxd, then fails, as lux.input failed, with the contract's error;
    the Run goes on."""
    runners.start(hosts[0])
    run_id = lux.submit(fake_agent(fake_image, "echo hi", workload={"command": ["lux-fake", "--no-images"]}))
    lux.wait_activity(run_id, "idle")
    img = tmp_path / "x.png"
    img.write_bytes(square_png(7))
    lux.run("steer", run_id, "echo with-image", "--image", str(img), "--request-id", "noimg")
    failed = wait_until(lambda: next((e["data"] for e in lux.events(run_id, "input.failed") if e["data"]["requestId"] == "noimg"), None),
                        30, 0.3, "the input never failed")
    assert failed["error"] == "the agent does not take images" and failed["phase"] == "failed", failed
    assert failed["attachments"] == [_meta("x.png", square_png(7))], failed
    lux.run("steer", run_id, "echo still-here")
    lux.wait_output(run_id, "still-here")
    assert "with-image" not in lux.logs(run_id)
    lux.run("cancel", run_id)
