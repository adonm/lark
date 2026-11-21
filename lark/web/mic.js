const vadSamples = 320; // 20 ms at 16 kHz.
const callSamples = 640; // 40 ms at 16 kHz.

class EchoMic extends AudioWorkletProcessor {
  constructor({processorOptions} = {}) {
    super();
    this.call = processorOptions?.mode === "call";
    this.bridgeEndpoint = processorOptions?.mode === "bridge";
    this.prefix = this.call && processorOptions.prefix === 4 ? 4 : 1;
    this.muted = this.call && !!processorOptions.muted;
    this.packetBytes = 1 + 2 * (this.call ? callSamples : vadSamples);
    this.ratio = sampleRate / 16000;
    this.left = this.ratio;
    this.sum = 0;
    this.packet = new Uint8Array(this.packetBytes);
    this.buffer = new DataView(this.packet.buffer);
    this.used = 1;
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
        this.used = 1;
        this.sum = 0;
        this.left = this.ratio;
        if (!this.muted) this.port.postMessage(data.seq);
      }
    };
  }
  finish(message) {
    if (!this.live) return;
    this.live = false;
    if (this.used > 1) this.send();
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
    const rms = Math.sqrt(this.energy / vadSamples);
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
    this.packet[0] = this.prefix;
    const frame = this.used === this.packet.length ? this.packet.buffer : this.packet.buffer.slice(0, this.used);
    this.port.postMessage(frame, [frame]);
    this.packet = new Uint8Array(this.packetBytes);
    this.buffer = new DataView(this.packet.buffer);
    this.used = 1;
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
