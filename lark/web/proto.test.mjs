import test from "node:test";
import assert from "node:assert/strict";
import {
  CAP, DISP, KIND, MSG, PROTO_VERSION, PTR, RTP_PT, SHAPE,
  decodeRTP, decodeServerMsg, encodeCallReq, encodeHello, encodePointer,
  encodePromptReply, encodeRecEnd, encodeRecStart, encodeResume, encodeRTP,
  encodeSetVolume, encodeTTSAck, encodeTextInput, encodeWatch, packFrame, unpackFrame,
} from "./proto.js";

test("frame pack and unpack", () => {
  const payload = new Uint8Array([1, 2, 3, 4]);
  const wire = packFrame(MSG.POINTER, 0x1234, payload);
  assert.equal(wire[0], 0x4c);
  assert.equal(wire[1], MSG.POINTER);
  assert.equal(wire.byteLength, 12);
  const f = unpackFrame(wire);
  assert.equal(f.type, MSG.POINTER);
  assert.equal(f.seq, 0x1234);
  assert.deepEqual([...f.payload], [1, 2, 3, 4]);
  assert.equal(unpackFrame(new Uint8Array([0x00, 1, 0, 0, 0, 0, 0, 0])), null);
});

test("hello, resume, pointer, rtp, and control encoders", () => {
  const hello = encodeHello({
    kind: KIND.WEB, caps: CAP.CALL | CAP.DRAG, displayFormats: DISP.SVG,
    shape: SHAPE.ROUND, terminalId: "web-11223344", companionId: "cake", token: "tok",
  });
  assert.equal(hello[0], PROTO_VERSION);
  assert.equal(hello[1], KIND.WEB);
  assert.equal(new DataView(hello.buffer).getUint16(8, true), 360);

  const tok = new Uint8Array(16).fill(0xab);
  const res = encodeResume({ resumeToken: tok, lastSeq: 99, terminalId: "web-11223344" });
  assert.equal(res[0], PROTO_VERSION);
  assert.equal(new DataView(res.buffer).getUint32(17, true), 99);

  const ptr = encodePointer("tap", 120, 240, 7);
  assert.equal(ptr.byteLength, 8);
  assert.equal(ptr[0], PTR.tap);

  const pcm = new Uint8Array(640).fill(0x11);
  const rtp = encodeRTP(RTP_PT.REC_PCM, 12, 320, 0xdeadbeef, pcm);
  assert.equal(rtp.byteLength, 652);
  const dec = decodeRTP(rtp);
  assert.equal(dec.pt, RTP_PT.REC_PCM);
  assert.equal(dec.seq, 12);
  assert.equal(dec.ts, 320);
  assert.equal(dec.ssrc, 0xdeadbeef);
  assert.equal(dec.pcm.byteLength, 640);

  assert.equal(encodeRecStart(1, "").byteLength, 3);
  assert.equal(encodeRecEnd(1, true, "").byteLength, 4);
  assert.equal(encodeTTSAck(false, 42).byteLength, 9);
  assert.equal(encodeCallReq(1, 0, "mint").byteLength, 10);
  assert.ok(encodeTextInput("hi").byteLength > 2);
  assert.ok(encodePromptReply("p1", "allow").byteLength > 2);
  assert.ok(encodeWatch("cake").byteLength > 1);
  assert.deepEqual([...encodeSetVolume(true, 60)], [1, 60]);
});

test("decodeServerMsg handles welcome, reject, status, voice_owner, prompt, frame_svg", () => {
  const enc = new TextEncoder();
  const w = new Uint8Array(38 + 10 + 4);
  const wdv = new DataView(w.buffer);
  w[0] = PROTO_VERSION; w[1] = 1;
  wdv.setUint16(2, 8766, true); wdv.setUint32(4, 0x12345678, true);
  wdv.setUint32(8, 320 * 1024, true); wdv.setUint32(12, 30000, true); wdv.setUint32(16, 7, true);
  w[36] = 10; w[37] = 4;
  w.set(enc.encode("web-aabbcc"), 38);
  w.set(enc.encode("cake"), 48);
  const welcome = decodeServerMsg(MSG.WELCOME, w);
  assert.equal(welcome.protoVersion, 1);
  assert.equal(welcome.bridgeEndpoint, true);
  assert.equal(welcome.seq, 7);
  assert.equal(welcome.terminalId, "web-aabbcc");
  assert.equal(welcome.companionId, "cake");

  const rej = new Uint8Array([2, 3, ...enc.encode("bad")]);
  assert.deepEqual(decodeServerMsg(MSG.REJECT, rej), { reason: "bad_version", code: 2, message: "bad" });

  const st = new Uint8Array(9 + 4);
  const stdv = new DataView(st.buffer);
  stdv.setUint32(0, 11, true); st[4] = 2; st[5] = 50; st[6] = 0; stdv.setUint16(7, 4, true);
  st.set(enc.encode("busy"), 9);
  assert.deepEqual(decodeServerMsg(MSG.STATUS, st), { seq: 11, state: "thinking", progress: 50, kind: "none", text: "busy" });

  const vo = new Uint8Array(4 + 1 + 4 + 1 + 10);
  new DataView(vo.buffer).setUint32(0, 12, true);
  vo[4] = 4; vo.set(enc.encode("cake"), 5);
  vo[9] = 10; vo.set(enc.encode("vocat-33d8"), 10);
  assert.deepEqual(decodeServerMsg(MSG.VOICE_OWNER, vo), { seq: 12, companionId: "cake", terminalId: "vocat-33d8" });
});
