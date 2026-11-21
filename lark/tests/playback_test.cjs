const assert = require("node:assert/strict");
const {test} = require("node:test");
const {audioFixture} = require("./audio_fixture.cjs");

test("schedules a 12-second reply gaplessly and acknowledges only after its final source ends", async () => {
  const {audio:{handleTTS, playPCM}, sources, events, context} = await audioFixture("tts");
  const packet = new Uint8Array(4001);
  packet[0] = 2;
  handleTTS({state:"start", audio:42});
  for (let i = 0; i < 8; i++) playPCM(packet.buffer);
  context.currentTime = .5;
  context.state = "suspended";
  context.onstatechange();
  for (let i = 8; i < 96; i++) playPCM(packet.buffer);
  handleTTS({state:"end", audio:42});
  assert.equal(sources.length, 96);
  assert.ok(sources[0].at > 0);
  for (const [i, s] of sources.entries()) {
    assert.ok(!s.stopped);
    if (i) assert.equal(s.at, sources[i-1].at + sources[i-1].buffer.duration);
  }
  context.state = "running";
  context.onstatechange();
  for (const s of sources.slice(0, -1)) s.onended();
  assert.deepEqual(events, []);
  sources.at(-1).onended();
  assert.deepEqual(events, [{ev:"tts_ack", state:"done", audio:42}]);
});

test("call plays and captures together, drops stale audio, and mutes only the uplink", async () => {
  const {audio:{handleCall, playPCM}, sources, events, tracks, constraints, nodes, state} = await audioFixture("call");
  const starting = handleCall({state:"start", call:17, muted:false, mic_prefix:4});
  const inPCM = new Uint8Array(1281), outPCM = new Uint8Array(1281);
  inPCM[0] = 2;
  outPCM[0] = 1;
  playPCM(inPCM.buffer);
  await starting;
  assert.ok(!sources[0].stopped);
  assert.deepEqual(constraints, [{audio:{channelCount:1, echoCancellation:true, noiseSuppression:true, autoGainControl:true}}]);
  assert.deepEqual(nodes[0].options.processorOptions, {mode:"call", prefix:4, muted:false});
  assert.deepEqual(events, []);
  for (let i = 0; i < 3; i++) playPCM(inPCM.buffer);
  assert.equal(sources.length, 4); // Playback continues with a live worklet.
  for (const source of sources) assert.ok(source.at + source.buffer.duration <= .18);
  assert.ok(sources[0].stopped); // The fourth packet would exceed 180 ms.
  assert.equal(sources[3].at, sources[0].at);
  nodes[0].port.onmessage({data:outPCM.buffer});
  handleCall({state:"mute",call:17,muted:true});
  nodes[0].port.onmessage({data:outPCM.buffer});
  playPCM(inPCM.buffer);
  assert.equal(sources.length, 5);
  assert.deepEqual(events, [outPCM.buffer]);
  handleCall({state:"mute",call:17,muted:false});
  assert.deepEqual(nodes[0].port.messages, [{muted:true,seq:1}, {muted:false,seq:2}]);
  nodes[0].port.onmessage({data:outPCM.buffer});
  nodes[0].port.onmessage({data:1}); // A stale acknowledgment must not unblock the mic.
  nodes[0].port.onmessage({data:outPCM.buffer});
  assert.deepEqual(events, [outPCM.buffer]);
  nodes[0].port.onmessage({data:2});
  nodes[0].port.onmessage({data:outPCM.buffer});
  state.ws.bufferedAmount = 10000;
  nodes[0].port.onmessage({data:outPCM.buffer});
  assert.deepEqual(events, [outPCM.buffer, outPCM.buffer]);
  handleCall({state:"end",call:17});
  assert.ok(tracks[0].stopped && nodes[0].port.closed && nodes[0].disconnected);
  assert.ok(sources.every(s => s.stopped));
  assert.deepEqual(events, [outPCM.buffer, outPCM.buffer]);
});

test("call requests resume while suspended and never schedules stale PCM", async () => {
  const {audio:{handleCall, playPCM}, sources, context} = await audioFixture("resume");
  await handleCall({state:"start", call:18});
  const packet = new Uint8Array(1281);
  packet[0] = 2;
  playPCM(packet.buffer);
  context.state = "suspended";
  let resumed, count = 0;
  context.resume = () => {
    count++;
    return new Promise(resolve => { resumed = resolve; });
  };
  for (let i = 0; i < 20; i++) playPCM(packet.buffer);
  assert.equal(count, 1);
  assert.equal(sources.length, 1);
  assert.ok(sources[0].stopped);
  context.currentTime = 1;
  context.state = "running";
  resumed();
  playPCM(packet.buffer);
  assert.equal(sources.length, 2);
  assert.equal(sources[1].at, 1.035);
  handleCall({state:"end", call:18});
});

test("ring uses two spaced tones and stops on answer", async () => {
  const {audio:{handleCall, playChime}, tones} = await audioFixture("ring");
  playChime("ring");
  assert.equal(tones.length, 2);
  assert.ok(tones[0].frequency.value < tones[1].frequency.value);
  assert.ok(tones[0].end < tones[1].at);
  const started = handleCall({state:"start", call:19});
  assert.ok(tones.every(osc => osc.end === undefined));
  playChime("ring");
  assert.equal(tones.length, 2);
  await started;
  handleCall({state:"end", call:19});
});
