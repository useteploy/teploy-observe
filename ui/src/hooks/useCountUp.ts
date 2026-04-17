import { useEffect, useState, useRef } from "preact/hooks";

// Animates a number from 0 to target with easeOut cubic over ~800ms.
// Only animates when target changes meaningfully.
export function useCountUp(target: number, duration = 800): number {
  const [current, setCurrent] = useState(target);
  const rafRef = useRef<number | null>(null);
  const startRef = useRef<number>(0);
  const fromRef = useRef<number>(0);

  useEffect(() => {
    if (typeof window === "undefined" || typeof target !== "number" || !isFinite(target)) {
      setCurrent(target);
      return;
    }

    // Skip animation for tiny changes
    if (Math.abs(current - target) < 1 && target !== 0) {
      setCurrent(target);
      return;
    }

    fromRef.current = current;
    startRef.current = performance.now();

    const tick = (now: number) => {
      const elapsed = now - startRef.current;
      const t = Math.min(elapsed / duration, 1);
      // easeOutCubic
      const eased = 1 - Math.pow(1 - t, 3);
      const value = fromRef.current + (target - fromRef.current) * eased;
      setCurrent(t >= 1 ? target : value);
      if (t < 1) rafRef.current = requestAnimationFrame(tick);
    };

    rafRef.current = requestAnimationFrame(tick);
    return () => {
      if (rafRef.current) cancelAnimationFrame(rafRef.current);
    };
  }, [target]);

  return current;
}
