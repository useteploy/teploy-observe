import { useEffect, useRef } from "preact/hooks";
import type { ComponentChildren } from "preact";

interface Props {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ComponentChildren;
}

export default function Modal({ open, onClose, title, children }: Props) {
  const modalRef = useRef<HTMLDivElement>(null);
  // Inline onClose props get a new identity on every parent re-render
  // (e.g. on each keystroke in a sibling form field). Reading it via ref
  // keeps that churn out of the effect's deps below, so the effect only
  // re-runs (and re-focuses the modal) when `open` itself changes.
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    if (!open) return;
    // Lock body scroll
    document.body.style.overflow = "hidden";
    // Focus the modal
    modalRef.current?.focus();

    const handleKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") { onCloseRef.current(); return; }
      // Focus trap
      if (e.key === "Tab" && modalRef.current) {
        const focusable = modalRef.current.querySelectorAll<HTMLElement>(
          'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'
        );
        if (focusable.length === 0) return;
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault(); last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault(); first.focus();
        }
      }
    };

    document.addEventListener("keydown", handleKey);
    return () => {
      document.body.style.overflow = "";
      document.removeEventListener("keydown", handleKey);
    };
  }, [open]);

  if (!open) return null;

  return (
    <div class="obs-modal-overlay" onClick={onClose} role="dialog" aria-modal="true" aria-label={title}>
      <div class="obs-modal" ref={modalRef} tabIndex={-1} onClick={(e) => e.stopPropagation()}>
        <div class="obs-modal-header">
          <h2 class="obs-modal-title">{title}</h2>
          <button class="obs-modal-close" onClick={onClose} aria-label="Close dialog">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor">
              <path d="M19 6.41L17.59 5 12 10.59 6.41 5 5 6.41 10.59 12 5 17.59 6.41 19 12 13.41 17.59 19 19 17.59 13.41 12z" />
            </svg>
          </button>
        </div>
        <div class="obs-modal-body">{children}</div>
      </div>
    </div>
  );
}
