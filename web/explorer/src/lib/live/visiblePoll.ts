/**
 * pollWhileVisible — run `tick` every `intervalMs`, skipping ticks while the
 * tab is hidden and catching up immediately when it becomes visible again.
 * Does not fire an initial tick; callers keep their own mount-time call.
 * Returns the cleanup function.
 */
export function pollWhileVisible(
  tick: () => void,
  intervalMs: number,
): () => void {
  let missed = false;
  const id = setInterval(() => {
    if (document.hidden) {
      missed = true;
      return;
    }
    tick();
  }, intervalMs);
  const onVisible = () => {
    if (document.hidden || !missed) return;
    missed = false;
    tick();
  };
  document.addEventListener('visibilitychange', onVisible);
  return () => {
    clearInterval(id);
    document.removeEventListener('visibilitychange', onVisible);
  };
}
