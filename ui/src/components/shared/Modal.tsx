import type { ComponentChildren } from "preact";

interface Props {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ComponentChildren;
}

export default function Modal({ open, onClose, title, children }: Props) {
  if (!open) return null;

  return (
    <div class="obs-modal-overlay" onClick={onClose}>
      <div class="obs-modal" onClick={(e) => e.stopPropagation()}>
        <div class="obs-modal-header">
          <h2 class="obs-modal-title">{title}</h2>
          <button class="obs-modal-close" onClick={onClose}>
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
