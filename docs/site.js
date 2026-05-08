// Copy-to-clipboard for the install command pills.
// Falls back to a manual textarea-select on browsers that don't support
// the async clipboard API (rare in 2026, but trivial to keep).
document.querySelectorAll('button.copy').forEach(btn => {
  btn.addEventListener('click', async () => {
    const text = btn.dataset.copy;
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      const ta = document.createElement('textarea');
      ta.value = text; document.body.appendChild(ta);
      ta.select(); document.execCommand('copy'); ta.remove();
    }
    const icon = btn.querySelector('.copy-icon');
    const original = icon.textContent;
    btn.classList.add('copied');
    icon.textContent = '✓';
    setTimeout(() => {
      btn.classList.remove('copied');
      icon.textContent = original;
    }, 1400);
  });
});

// Hero video play/pause toggle. Required for WCAG 2.2 SC 2.2.2 — any
// auto-playing media longer than 5s must offer a pause control. Also
// auto-pauses when the OS reports prefers-reduced-motion so vestibular
// users are not assaulted on first paint.
(() => {
  const video = document.getElementById('hero-video');
  const toggle = document.querySelector('.video-toggle');
  if (!video || !toggle) return;

  const setPressed = (playing) => {
    toggle.setAttribute('aria-pressed', playing ? 'true' : 'false');
    toggle.setAttribute('aria-label', playing ? 'Pause demo video' : 'Play demo video');
  };

  const reduce = window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)');
  const syncMotionPreference = () => {
    if (reduce && reduce.matches) {
      video.pause();
      setPressed(false);
      return;
    }
    video.play().catch(() => {});
    setPressed(!video.paused);
  };

  if (reduce && reduce.matches) {
    video.pause();
    setPressed(false);
  } else {
    // Autoplay only when reduced-motion is not requested.
    video.play().catch(() => {});
    setPressed(!video.paused);
  }

  if (reduce && typeof reduce.addEventListener === 'function') {
    reduce.addEventListener('change', syncMotionPreference);
  }

  toggle.addEventListener('click', () => {
    if (video.paused) {
      video.play().catch(() => {});
    } else {
      video.pause();
    }
  });

  video.addEventListener('play', () => setPressed(true));
  video.addEventListener('pause', () => setPressed(false));
})();
