import Modal from "./Modal.js";

interface Props {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  title: string;
  message: string;
  confirmLabel?: string;
  loading?: boolean;
}

export default function ConfirmDialog({ open, onClose, onConfirm, title, message, confirmLabel, loading }: Props) {
  return (
    <Modal open={open} onClose={onClose} title={title}>
      <p style={{ fontSize: "13px", color: "var(--obs-text-secondary)", margin: "0 0 16px" }}>{message}</p>
      <div style={{ display: "flex", justifyContent: "flex-end", gap: "8px" }}>
        <button class="obs-btn" onClick={onClose} disabled={loading}>Cancel</button>
        <button class="obs-btn obs-btn--danger" onClick={onConfirm} disabled={loading}>
          {loading ? "Deleting..." : (confirmLabel || "Delete")}
        </button>
      </div>
    </Modal>
  );
}
