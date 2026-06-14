// rooms-blur.js — real-time camera background blur via MediaPipe Selfie
// Segmentation. Wraps an input MediaStream (one camera video track) and
// returns a new MediaStream whose video track is the same person on a
// blurred background, ready to addTrack onto a PeerConnection.
//
// Pipeline:
//   getUserMedia → <video> → segmenter.send({image: video})
//     → onResults({segmentationMask, image})
//       → canvas: draw person sharp, blurred image underneath
//       → canvas.captureStream() → outbound track
//
// Failure modes:
//   - jsdelivr script load fails or the user is offline → throws; caller
//     falls back to the raw camera stream.
//   - Browser lacks canvas.captureStream / canvas filter → throws.
//
// Exports a single function on window.fcRoomsBlur:
//   wrap(rawStream, { blurPx = 12 }) → Promise<{ stream, stop() }>
(() => {
  const MP_VERSION = '0.1.1675465747';
  const MP_BASE = `https://cdn.jsdelivr.net/npm/@mediapipe/selfie_segmentation@${MP_VERSION}`;
  const SCRIPT_URL = `${MP_BASE}/selfie_segmentation.js`;

  let scriptPromise = null;

  function loadMediaPipe() {
    if (window.SelfieSegmentation) return Promise.resolve();
    if (scriptPromise) return scriptPromise;
    scriptPromise = new Promise((resolve, reject) => {
      const s = document.createElement('script');
      s.src = SCRIPT_URL;
      s.crossOrigin = 'anonymous';
      s.onload = () => resolve();
      s.onerror = () => reject(new Error('selfie_segmentation script failed to load'));
      document.head.appendChild(s);
    });
    return scriptPromise;
  }

  async function wrap(rawStream, opts) {
    const blurPx = opts?.blurPx ?? 12;

    // Sanity — require a usable platform.
    if (typeof HTMLCanvasElement === 'undefined'
        || typeof HTMLCanvasElement.prototype.captureStream !== 'function') {
      throw new Error('canvas.captureStream not supported');
    }

    await loadMediaPipe();

    const videoIn = document.createElement('video');
    videoIn.srcObject = rawStream;
    videoIn.muted = true;
    videoIn.playsInline = true;
    videoIn.autoplay = true;
    try { await videoIn.play(); } catch (_) { /* ignore — play() can reject in some flows */ }

    // Wait for real dimensions before sizing the canvas.
    if (!videoIn.videoWidth) {
      await new Promise((r) => videoIn.addEventListener('loadedmetadata', r, { once: true }));
    }
    const w = videoIn.videoWidth || 640;
    const h = videoIn.videoHeight || 480;

    const canvas = document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('2d context unavailable');

    const segmenter = new window.SelfieSegmentation({
      locateFile: (file) => `${MP_BASE}/${file}`,
    });
    // modelSelection: 0 = general (faster), 1 = landscape (higher fidelity).
    // 1 is a noticeably better head/shoulders cut for a typical webcam shot.
    segmenter.setOptions({ modelSelection: 1, selfieMode: false });

    segmenter.onResults((results) => {
      // Compositing pattern:
      //   1. Draw the mask. Wherever it is opaque is "person".
      //   2. Replace those pixels with the sharp image (source-in).
      //   3. Paint the blurred image UNDER everything else
      //      (destination-over + canvas filter blur).
      ctx.save();
      ctx.clearRect(0, 0, w, h);
      ctx.drawImage(results.segmentationMask, 0, 0, w, h);
      ctx.globalCompositeOperation = 'source-in';
      ctx.drawImage(results.image, 0, 0, w, h);
      ctx.globalCompositeOperation = 'destination-over';
      ctx.filter = `blur(${blurPx}px)`;
      ctx.drawImage(results.image, 0, 0, w, h);
      ctx.filter = 'none';
      ctx.restore();
    });

    let running = true;
    async function tick() {
      if (!running) return;
      try {
        await segmenter.send({ image: videoIn });
      } catch (e) {
        console.warn('[rooms-blur] segmenter.send failed', e);
      }
      // Pace via rAF — runs as fast as the display, throttled by the browser
      // when the tab is hidden so we don't burn battery in the background.
      if (running) requestAnimationFrame(tick);
    }
    tick();

    const outStream = canvas.captureStream(24);

    function stop() {
      running = false;
      try { segmenter.close(); } catch (_) {}
      try { videoIn.srcObject = null; } catch (_) {}
      // Stop output tracks so receivers see an ended event.
      outStream.getTracks().forEach((t) => { try { t.stop(); } catch (_) {} });
    }

    return { stream: outStream, stop };
  }

  window.fcRoomsBlur = { wrap };
})();
