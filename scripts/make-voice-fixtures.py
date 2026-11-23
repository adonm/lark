#!/usr/bin/env python3
"""Generate the synthetic voice fixtures in lark/qa/fixtures/voice.

These clips exercise the endpoint shapes from docs/qa-report.md: short and long
turns, a trailing filler, a mid-sentence pause, a comma list, a function-word
ending, quiet speech over pink noise, noise only, and a 20 s monologue.

Everything here is TTS output (Kokoro through the local Lark bridge) plus
deterministic synthetic noise. Never write a real household capture into the
tracked fixture directory: put it in qa/fixtures/local/ instead (gitignored).

Usage:
    scripts/make-voice-fixtures.py [--url http://127.0.0.1:8765]

The bridge token is read from LARK_TOKEN or the installed lark.env.
"""
import argparse
import array
import json
import math
import os
import random
import sys
import urllib.request
import wave

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "lark", "qa", "fixtures", "voice")
RATE = 16000


def token():
    if os.environ.get("LARK_TOKEN"):
        return os.environ["LARK_TOKEN"]
    home = os.path.expanduser("~")
    path = os.path.join(os.environ.get("XDG_CONFIG_HOME", os.path.join(home, ".config")), "lark", "lark.env")
    with open(path) as f:
        for line in f:
            key, _, value = line.strip().partition("=")
            if key == "LARK_TOKEN":
                return value.strip().strip("'\"")
    raise SystemExit("set LARK_TOKEN or install lark.env")


def tts(base, text):
    req = urllib.request.Request(
        base + "/api/tts",
        data=json.dumps({"text": text}).encode(),
        headers={"Content-Type": "application/json", "X-Lark-Token": token()},
    )
    raw = urllib.request.urlopen(req, timeout=60).read()
    with wave.open(__import__("io").BytesIO(raw)) as w:
        if w.getframerate() != RATE or w.getnchannels() != 1 or w.getsampwidth() != 2:
            raise SystemExit("bridge TTS must return 16 kHz mono PCM16")
        samples = array.array("h")
        samples.frombytes(w.readframes(w.getnframes()))
    return samples


def silence(ms):
    return array.array("h", [0] * (RATE * ms // 1000))


def pink_noise(seconds, rms, seed):
    # Paul Kellet's economy pink filter, then scale to the requested RMS.
    rnd = random.Random(seed)
    out = array.array("h", [0]) * 0
    b = [0.0] * 7
    raw = []
    for _ in range(RATE * seconds):
        white = rnd.uniform(-1, 1)
        b[0] = 0.99886 * b[0] + white * 0.0555179
        b[1] = 0.99332 * b[1] + white * 0.0750759
        b[2] = 0.96900 * b[2] + white * 0.1538520
        b[3] = 0.86650 * b[3] + white * 0.3104856
        b[4] = 0.55000 * b[4] + white * 0.5329522
        b[5] = -0.7616 * b[5] - white * 0.0168980
        value = b[0] + b[1] + b[2] + b[3] + b[4] + b[5] + b[6] + white * 0.5362
        b[6] = white * 0.115926
        raw.append(value)
    peak = max(abs(v) for v in raw) or 1
    unit = math.sqrt(sum((v / peak) ** 2 for v in raw) / len(raw)) or 1
    gain = (rms / unit) / peak
    for v in raw:
        out.append(max(-rms * 3, min(rms * 3, int(v * gain))))
    return out


def scale(samples, factor):
    return array.array("h", [max(-32768, min(32767, int(v * factor))) for v in samples])


def add(a, b):
    out = array.array("h", [0] * max(len(a), len(b)))
    for i in range(len(out)):
        va = a[i] if i < len(a) else 0
        vb = b[i] if i < len(b) else 0
        out[i] = max(-32768, min(32767, va + vb))
    return out


def write(name, samples):
    path = os.path.join(OUT, name)
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(RATE)
        w.writeframes(samples.tobytes())
    return {"file": name, "ms": len(samples) * 1000 // RATE,
            "rms": round(math.sqrt(sum(v * v for v in samples[::16]) / max(1, len(samples[::16]))))}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:8765")
    args = parser.parse_args()
    os.makedirs(OUT, exist_ok=True)
    base = args.url.rstrip("/")

    def t(text):
        return tts(base, text)

    fixtures = []

    def add_fixture(name, samples, **fields):
        info = write(name, samples)
        entry = {"name": name.rsplit(".", 1)[0], **info, "origin": "synthetic", **fields}
        fixtures.append(entry)

    short = t("What is two plus two?")
    add_fixture("short.wav", short + silence(600), kind="short",
                transcript="What is two plus two?",
                progress=[{"at_ms": info_ms(short), "text": "What is two plus two?"}],
                expected={"endpoint": "quiet", "commit_after_last_voiced_ms": 400})

    long_text = ("Please tell me a long and winding story about a small green dragon "
                 "who lived in a teapot at the edge of a quiet village near the sea.")
    long = t(long_text)
    add_fixture("long.wav", long + silence(600), kind="long",
                transcript=long_text, progress=[{"at_ms": info_ms(long), "text": long_text}],
                expected={"endpoint": "quiet", "commit_after_last_voiced_ms": 400,
                          "min_speech_ms": 4000})

    timer = t("Set a timer for ninety seconds.")
    add_fixture("timer.wav", timer + silence(600), kind="command",
                transcript="Set a timer for ninety seconds.",
                progress=[{"at_ms": info_ms(timer), "text": "Set a timer for ninety seconds."}],
                expected={"endpoint": "quiet", "commit_after_last_voiced_ms": 400})

    um = t("Show me a giraffe, um.")
    add_fixture("trailing-um.wav", um + silence(1500), kind="filler",
                transcript="Show me a giraffe, um.",
                progress=[{"at_ms": info_ms(um), "text": "Show me a giraffe, um."}],
                expected={"endpoint": "cap", "commit_after_last_voiced_ms": 1200,
                          "incomplete": True})

    first = t("Once upon a time there was a little frog, and then")
    second = t("it made a house under a purple mushroom.")
    pause = first + silence(900) + second
    add_fixture("pause.wav", pause + silence(600), kind="pause",
                transcript="Once upon a time there was a little frog, and then it made a house under a purple mushroom.",
                progress=[{"at_ms": info_ms(first), "text": "Once upon a time there was a little frog, and then"},
                          {"at_ms": info_ms(first) + 900 + info_ms(second),
                           "text": "Once upon a time there was a little frog, and then it made a house under a purple mushroom."}],
                expected={"endpoint": "quiet", "commit_after_last_voiced_ms": 400,
                          "no_commit_ms": [info_ms(first) + 400, info_ms(first) + 850]})

    items = t("I want apples, grapes, and strawberries and chocolate sprinkles too!")
    add_fixture("list.wav", items + silence(600), kind="list",
                transcript="I want apples, grapes, and strawberries and chocolate sprinkles too!",
                progress=[{"at_ms": info_ms(items),
                           "text": "I want apples, grapes, and strawberries and chocolate sprinkles too!"}],
                expected={"endpoint": "quiet", "commit_after_last_voiced_ms": 400})

    his = t("Can you put his")
    add_fixture("function-word.wav", his + silence(1500), kind="function-word",
                transcript="Can you put his",
                progress=[{"at_ms": info_ms(his), "text": "Can you put his"}],
                expected={"endpoint": "cap", "commit_after_last_voiced_ms": 1200,
                          "incomplete": True})

    quiet_text = "Turn on the little lamp please."
    quiet = scale(t(quiet_text), 0.22)
    noise = pink_noise(3, 110, 7)
    quiet_mix = add(quiet, noise[:len(quiet)]) + noise[len(quiet):] if len(noise) > len(quiet) else add(quiet, noise)
    add_fixture("quiet-over-noise.wav", quiet_mix + silence(600), kind="quiet",
                transcript=quiet_text, progress=[{"at_ms": info_ms(quiet), "text": quiet_text}],
                expected={"endpoint": "quiet", "commit_after_last_voiced_ms": 400,
                          "speech_rms_over_floor": True})

    noise_only = pink_noise(3, 70, 11)
    add_fixture("noise-only.wav", noise_only, kind="noise",
                transcript="", progress=[],
                expected={"endpoint": "discard"})

    sentences = [
        "Let me tell you about the long trip we took to the mountains last summer.",
        "We packed a red tent, two sleeping bags, a little stove, and far too many apples.",
        "The road climbed past a waterfall, a wooden bridge, and a field full of yellow flowers.",
        "At the top the wind was cold, so we made cocoa and watched the clouds roll below us.",
        "On the way home we found a bakery that sold cinnamon rolls bigger than my hand.",
        "I would go back tomorrow if the car were not still full of pine needles and sand.",
    ]
    mono = array.array("h", [])
    for i, line in enumerate(sentences):
        clip = t(line)
        if len(mono) * 1000 // RATE >= 19800:
            break
        mono += clip
        if i + 1 < len(sentences) and len(mono) * 1000 // RATE < 19500:
            mono += silence(60)
    add_fixture("monologue-20s.wav", mono + silence(300), kind="monologue",
                transcript=" ".join(sentences),
                progress=[{"at_ms": info_ms(mono), "text": " ".join(sentences)}],
                expected={"endpoint": "monologue", "commit_after_last_voiced_ms": 0,
                          "min_speech_ms": 16000})

    manifest = {"sample_rate": RATE, "note": "Synthetic TTS fixtures only; local captures are never tracked.",
                "fixtures": fixtures}
    with open(os.path.join(OUT, "manifest.json"), "w") as f:
        json.dump(manifest, f, indent=2)
        f.write("\n")
    total = sum(os.path.getsize(os.path.join(OUT, f["file"])) for f in fixtures)
    print(f"wrote {len(fixtures)} fixtures, {total / 1e6:.2f} MB, to {OUT}")


def info_ms(samples):
    return len(samples) * 1000 // RATE


if __name__ == "__main__":
    main()
