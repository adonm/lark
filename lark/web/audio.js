import {$, say} from "./dom.js";
import {MSG, RTP_PT, decodeRTP, encodeCallReq, encodeRecEnd, encodeRecStart, encodeTTSAck} from "./proto.js";
import {sendProto, state} from "./state.js";

let audio, gain, playAt = 0, volume = 55, endedAudio = null, muted = false, call = null;
const speaking = new Set();
const ringing = new Set();
function speakerUI() {
  say("sound", audio?.state !== "running" ? "Enable speaker" : muted ? "Unmute speaker" : "Mute speaker");
}
function setVolume(n) {
  volume = n;
  $("volume").value = n;
  $("volume-value").value = n + "%";
  if (gain) gain.gain.value = muted ? 0 : n / 100;
  syncSpeaker();
}
function canPlay() {
  return audio?.state === "running" && canBufferPCM();
}
function canBufferPCM() {
  return audio && audio.state !== "closed" && !muted && volume > 0;
}
function syncSpeaker() {
  speakerUI();
  // Keep queued TTS across a suspension, but never replay stale call audio.
  if (!canBufferPCM() || call && audio.state !== "running") {
    clearPCM();
    ackIfDone();
  }
}
async function unlockAudio() {
  try {
    if (!audio) {
      audio = new AudioContext();
      gain = audio.createGain();
      gain.gain.value = muted ? 0 : volume / 100;
      gain.connect(audio.destination);
      audio.onstatechange = syncSpeaker;
    }
    if (audio.state !== "running") await audio.resume();
    speakerUI();
    return audio.state === "running";
  } catch {
    return false;
  }
}
$("sound").onclick = async () => {
  if (audio?.state === "running") {
    muted = !muted;
    setVolume(volume);
  } else if (!await unlockAudio()) {
    say("chat-note", "The browser couldn’t enable audio.");
  }
};
function schedulePCM(pcmBytes) {
  if (!(pcmBytes instanceof Uint8Array) || pcmBytes.byteLength < 2 || pcmBytes.byteLength % 2 !== 0) return;
  if (!canBufferPCM() || mic && !call || call?.problem) return;
  if (call && audio.state === "suspended") {
    if (!call.resuming) {
      const active = call;
      active.resuming = true;
      clearPCM();
      unlockAudio().finally(() => { active.resuming = false; });
    }
    return;
  }
  const wire = new DataView(pcmBytes.buffer, pcmBytes.byteOffset, pcmBytes.byteLength), count = wire.byteLength / 2;
  const now = audio.currentTime;
  if (call) {
    if (!canPlay() || count > 640) return;
    // TTS may queue seconds of PCM; a call discards any stale lead.
    if (playAt - now + count / 16000 > .18) clearPCM();
  }
  const buffer = audio.createBuffer(1,count,16000), samples = buffer.getChannelData(0);
  for (let i = 0; i < count; i++) samples[i] = wire.getInt16(i*2,true)/32768;
  const source = audio.createBufferSource();
  source.buffer = buffer;
  source.connect(gain);
  if (playAt <= now) playAt = now + .035;
  source.start(playAt);
  playAt += buffer.duration;
  speaking.add(source);
  source.onended = () => {
    speaking.delete(source);
    source.disconnect();
    ackIfDone();
    if (!speaking.size) audioActivityChanged?.();
  };
}
function playPCM(raw) {
  const bytes = raw instanceof Uint8Array ? raw : new Uint8Array(raw || 0);
  schedulePCM(bytes);
}
function playMedia(raw) {
  const rtp = decodeRTP(raw);
  if (rtp && rtp.pt === RTP_PT.CALL_DOWN) schedulePCM(rtp.pcm);
}
function ackIfDone() {
  if (endedAudio !== null && speaking.size === 0) {
    sendProto(MSG.TTS_ACK, encodeTTSAck(false, endedAudio));
    endedAudio = null;
  }
}
function clearPCM() {
  for (const source of speaking) {
    source.onended = null;
    source.stop();
    source.disconnect();
  }
  speaking.clear();
  playAt = 0;
}
function stopSound() {
  endedAudio = null;
  clearPCM();
}
function handleTTS(msg) {
  if (call) return;
  if (msg.state === "start") endedAudio = null;
  if (msg.state === "end") {
    endedAudio = msg.audio ?? 0;
    ackIfDone();
  }
  if (msg.state === "abort") stopSound();
}
function stopRing() {
  for (const osc of ringing) osc.stop();
  ringing.clear();
}
function playChime(kind) {
  if (call) return;
  if (kind === "cancel") stopSound();
  if (!canPlay()) return;
  const ring = kind === "ring", length = ring ? .14 : .09;
  const tones = {tap:[540],think:[440],intercom:[660,880],ring:[523.25,783.99],cancel:[280]}[kind] || [];
  if (ring) stopRing();
  for (const [i, hz] of tones.entries()) {
    const at = audio.currentTime+.025+i*(length+.01), osc = audio.createOscillator(), env = audio.createGain();
    osc.frequency.value = hz;
    env.gain.setValueAtTime(.001,at);
    env.gain.exponentialRampToValueAtTime(.10,at+.012);
    env.gain.exponentialRampToValueAtTime(.001,at+length-.005);
    osc.connect(env).connect(gain);
    if (ring) ringing.add(osc);
    osc.onended = () => {
      ringing.delete(osc);
      osc.disconnect();
      env.disconnect();
    };
    osc.start(at);
    osc.stop(at+length);
  }
}

let mic, workletLoaded;
let audioActivityChanged;
function onAudioActivityChanged(handler) { audioActivityChanged = handler; }
function micStopping() { return !!mic?.stopping; }
function audioBusy() { return !!mic || !!call || speaking.size > 0; }
function canCaptureMic() {
  return window.isSecureContext && typeof navigator.mediaDevices?.getUserMedia === "function" && !!window.AudioWorkletNode;
}
function recordUI(active) {
  $("talk").setAttribute("aria-pressed", String(active));
  $("talk").hidden = active;
  $("mic-send").hidden = !active;
  $("mic-send").disabled = true;
  $("mic-send").textContent = "Send now";
  $("cancel").hidden = !active;
  $("target").disabled = active;
}
function failCall(inCall, notice) {
  inCall.problem = notice;
  stopSound();
  say("chat-note", notice);
  sendProto(MSG.CALL_REQ, encodeCallReq(0, inCall.id, ""));
}
async function startMic(target, inCall = null, endpoint = "") {
  if (mic || call && !inCall) return;
  if (!canCaptureMic()) {
    $("mic-help").hidden = false;
    if (inCall) {
      failCall(inCall, window.isSecureContext ? "Call ended: this browser can’t capture microphone audio." :
        "Call ended: microphone access requires HTTPS or localhost.");
    } else sendProto(MSG.REC_END, encodeRecEnd(1, true, target));
    return;
  }
  if (!inCall) {
    clearPCM();
    ackIfDone();
  }
  const rec = {target,call:inCall,stream:null,node:null,source:null,stopping:false,bridgeEndpoint:endpoint === "bridge"};
  mic = rec;
  if (!inCall) {
    recordUI(true);
    audioActivityChanged?.();
    if (rec.bridgeEndpoint) say("chat-note", "Opening microphone…");
  }
  try {
    const ready = unlockAudio();
    // Autoplay policy can leave resume pending. Still ask for mic permission
    // now so a failure can end the call instead of leaving silent endpoints.
    if ((inCall ? !gain || audio.state === "closed" : !await ready) || !state.view?.web && !state.view?.virtual ||
        mic !== rec || state.ws?.readyState !== WebSocket.OPEN) throw new Error();
    rec.stream = await navigator.mediaDevices.getUserMedia({
      audio:{channelCount:1, echoCancellation:true, noiseSuppression:!!inCall, autoGainControl:!!inCall},
    });
    if (mic !== rec) { rec.stream.getTracks().forEach(t=>t.stop()); return; }
    workletLoaded ||= audio.audioWorklet.addModule("/app/mic.js").catch(err => { workletLoaded = null; throw err; });
    await workletLoaded;
    if (mic !== rec) return;
    for (const track of rec.stream.getTracks()) track.addEventListener?.("ended", () => {
      if (mic === rec) endMic(true);
    });
    rec.source = audio.createMediaStreamSource(rec.stream);
    rec.node = new AudioWorkletNode(audio,"echo-mic",{
      channelCount:1, channelCountMode:"explicit", processorOptions:inCall ?
        {mode:"call", ssrc:state.ssrc || 1, muted:inCall.muted} : {mode:rec.bridgeEndpoint ? "bridge" : "note", ssrc:state.ssrc || 1},
    });
    rec.node.port.onmessage = event => {
      if (mic !== rec) return;
      if (inCall) {
        if (inCall.waiting && event.data === inCall.waiting) inCall.waiting = 0;
        if (event.data instanceof ArrayBuffer && !inCall.muted && !inCall.waiting && state.ws?.readyState === WebSocket.OPEN &&
            state.ws.bufferedAmount < 6 * 660) sendProto(MSG.MEDIA, new Uint8Array(event.data));
      } else if (event.data === "end" || event.data === "auto_end") {
        endMic(false, true, true);
      } else if (state.ws?.readyState === WebSocket.OPEN) {
        if (state.ws.bufferedAmount > 256*1024) {
          endMic(true);
          say("chat-note", "Recording cancelled: the connection is too slow.");
        } else if (event.data instanceof ArrayBuffer) {
          sendProto(MSG.MEDIA, new Uint8Array(event.data));
        }
      }
    };
    if (!inCall) sendProto(MSG.REC_START, encodeRecStart(1, target));
    rec.source.connect(rec.node).connect(audio.destination);
    if (!inCall) {
      $("mic-send").disabled = false;
      say("chat-note", target ? "Recording intercom…" : "Listening…");
    }
  } catch {
    if (mic === rec) {
      endMic(true, !inCall);
      const notice = "Could not open the microphone. Check browser permissions.";
      if (inCall) failCall(inCall, "Call ended: " + notice);
      else say("chat-note", notice);
    }
  }
}
function endMic(cancelled, tell = true, flushed = false, fromBridge = false) {
  if (!mic) return;
  const rec = mic;
  if (!rec.call && rec.node && !cancelled && !flushed && !(fromBridge && rec.bridgeEndpoint)) {
    if (!rec.stopping) {
      rec.stopping = true;
      $("mic-send").disabled = true;
      $("mic-send").textContent = "Sending…";
      rec.node.port.postMessage("flush");
    }
    return;
  }
  mic = null;
  rec.stream?.getTracks().forEach(t => t.stop());
  rec.source?.disconnect();
  rec.node?.disconnect();
  rec.node?.port.close();
  if (rec.call) {
    if (tell) sendProto(MSG.CALL_REQ, encodeCallReq(0, rec.call.id, ""));
    return;
  }
  recordUI(false);
  audioActivityChanged?.();
  if (tell) {
    cancelled ||= !rec.node;
    sendProto(MSG.REC_END, encodeRecEnd(1, cancelled, rec.target));
    say("chat-note", cancelled ? "Recording cancelled." : "Message sent to the bridge.");
  }
}

function stopCall() {
  if (mic?.call) endMic(true, false);
  call = null;
  stopRing();
  stopSound();
}

function handleCall(msg) {
  if (msg.state === "start") {
    if (call?.id === msg.call) return;
    endMic(true, false);
    stopCall();
    call = {id:msg.call, muted:!!msg.muted, muteSeq:0, waiting:0};
    say("chat-note", "Voice call connected.");
    return startMic("", call);
  }
  if (call && call.id !== msg.call) return;
  if (msg.state === "mute" || msg.state === "unmute") {
    const isMuted = msg.state === "mute";
    if (call && call.muted !== isMuted) {
      call.muted = isMuted;
      if (mic?.call === call && mic.node) {
        const seq = ++call.muteSeq;
        // Ignore already queued worklet PCM until it acknowledges the reset.
        call.waiting = call.muted ? 0 : seq;
        mic.node.port.postMessage({muted:call.muted, seq});
      }
    }
  }
  if (msg.state === "end") {
    const notices = {"declined":"Call declined.", "no answer":"No answer.", "disconnected":"Peer disconnected.",
      "audio unavailable":"Call ended: audio unavailable."};
    say("chat-note", call?.problem || notices[msg.reason] || "Call ended.");
    stopCall();
  }
}

function requestMic(target) {
  if (!target && state.view?.bridgeEndpoint) {
    unlockAudio(); // Preserve the user gesture before the bridge's start reply.
    sendProto(MSG.REC_START, encodeRecStart(1, ""));
  } else startMic(target);
}
$("talk").onclick = () => requestMic($("target").value);
$("mic-send").onclick = () => endMic(false);
$("cancel").onclick = () => endMic(true);

export {audioBusy, canCaptureMic, endMic, handleCall, handleTTS, micStopping, onAudioActivityChanged, playChime, playMedia, playPCM,
  setVolume, startMic, stopCall, unlockAudio, volume};
