const rtpHdrBytes = 12;
const frameSamples = 320; // 20 ms at 16 kHz.
const ptRecPCM = 96;
const ptCallUp = 97;

class EchoMic extends AudioWorkletProcessor {
  constructor({processorOptions} = {}) {
    super();
    this.call = processorOptions?.mode === "call";
    this.bridgeEndpoint = processorOptions?.mode === "bridge";
    this.pt = this.call ? ptCallUp : ptRecPCM;
    this.ssrc = (processorOptions?.ssrc >>> 0) || 1;
    this.muted = this.call && !!processorOptions.muted;
    this.packetBytes = rtpHdrBytes + 2 * frameSamples;
    this.ratio = sampleRate / 16000;
    this.left = this.ratio;
    this.sum = 0;
    this.rtpSeq = 0;
    this.rtpTS = 0;
    this.packet = new Uint8Array(this.packetBytes);
    this.buffer = new DataView(this.packet.buffer);
    this.used = rtpHdrBytes;
    this.live = true;
    this.energy = 0;
    this.frames = 0;
    this.calibration = [];
    this.threshold = 0;
    this.voiced = 0;
    this.quiet = 0;
    this.port.onmessage = ({data}) => {
      if (!this.call) this.finish("end");
      else if (typeof data?.muted === "boolean") {
        this.muted = data.muted;
        this.used = rtpHdrBytes;
        this.sum = 0;
        this.left = this.ratio;
        if (!this.muted) this.port.postMessage(data.seq);
      }
    };
  }
  finish(message) {
    if (!this.live) return;
    this.live = false;
    if (this.used > rtpHdrBytes) this.send();
    this.port.postMessage(message);
  }
  hear(rms) {
    if (rms >= (this.voiced ? this.threshold * 0.5 : this.threshold)) {
      this.voiced++;
      this.quiet = 0;
    } else if (this.voiced) {
      this.quiet++;
    }
  }
  frame() {
    const rms = Math.sqrt(this.energy / frameSamples);
    this.energy = 0;
    this.frames++;
    if (this.frames <= 10) {
      this.calibration.push(rms);
      if (this.frames !== 10) return;
      // Estimate the floor from the quietest frames, then detect any early speech.
      const [a, b, c] = this.calibration.slice().sort((x, y) => x - y);
      this.threshold = Math.max(0.01, (a + b + c) / 3 * 2.5);
      for (const level of this.calibration) this.hear(level);
    } else {
      this.hear(rms);
    }
    // Bridge mode retains only the local privacy watchdog.
    const silence = this.bridgeEndpoint ? 400 : this.voiced < 200 ? 75 : 150;
    if (this.quiet >= silence || this.frames >= 3000 || (!this.voiced && this.frames >= 400)) {
      this.finish("auto_end");
    }
  }
  send() {
    const samples = (this.used - rtpHdrBytes) >> 1;
    this.buffer.setUint8(0, 0x80);
    this.buffer.setUint8(1, this.pt);
    this.buffer.setUint16(2, this.rtpSeq++ & 0xffff, false);
    this.buffer.setUint32(4, this.rtpTS >>> 0, false);
    this.buffer.setUint32(8, this.ssrc, false);
    this.rtpTS = (this.rtpTS + samples) >>> 0;
    const frame = this.used === this.packet.length ? this.packet.buffer : this.packet.buffer.slice(0, this.used);
    this.port.postMessage(frame, [frame]);
    this.packet = new Uint8Array(this.packetBytes);
    this.buffer = new DataView(this.packet.buffer);
    this.used = rtpHdrBytes;
  }
  process(inputs) {
    if (!this.live) return false;
    if (this.muted) return true;
    const mono = inputs[0]?.[0];
    if (!mono) return true;
    for (const value of mono) {
      let weight = 1;
      // Average across each destination sample's interval, carrying fractional
      // input and packet boundaries over to the next render quantum.
      while (weight > 1e-8) {
        const part = Math.min(weight, this.left);
        this.sum += value * part;
        this.left -= part;
        weight -= part;
        if (this.left <= 1e-8) {
          const sample = Math.max(-1, Math.min(1, this.sum / this.ratio));
          this.buffer.setInt16(this.used, Math.round(sample * (sample < 0 ? 32768 : 32767)), true);
          this.used += 2;
          if (!this.call) this.energy += sample * sample;
          if (this.used === this.packet.length) {
            this.send();
            if (!this.call) this.frame();
            if (!this.live) return false;
          }
          this.left = this.ratio;
          this.sum = 0;
        }
      }
    }
    return true;
  }
}

registerProcessor("echo-mic", EchoMic);
