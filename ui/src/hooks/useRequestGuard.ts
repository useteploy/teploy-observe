import { useCallback, useEffect, useRef } from "preact/hooks";

export function useRequestGuard(key: string) {
  const state = useRef({ key, generation: 0, mounted: true });
  if (state.current.key !== key) {
    state.current.key = key;
    state.current.generation++;
  }
  useEffect(() => {
    state.current.mounted = true;
    return () => { state.current.mounted = false; state.current.generation++; };
  }, []);
  return useCallback(() => {
    const generation = ++state.current.generation;
    return () => state.current.mounted && state.current.generation === generation;
  }, []);
}
