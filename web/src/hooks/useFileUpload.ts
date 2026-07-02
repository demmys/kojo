import { useCallback, useRef, useState } from "react";
import type { AgentMessageAttachment } from "../lib/agentApi";
import { api } from "../lib/api";

/**
 * Owns the composer's file-attachment lifecycle: a hidden <input> ref, the
 * pending (uploaded but not-yet-sent) attachments, and the upload/remove
 * handlers. Uploads run concurrently; partial failures surface as an error
 * string while successful uploads still attach. The hook does not reset on
 * any key change — callers that need per-conversation reset call the exposed
 * setters explicitly.
 */
export function useFileUpload() {
  const [pendingFiles, setPendingFiles] = useState<AgentMessageAttachment[]>([]);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);

  const handleFileSelect = useCallback(async (e: React.ChangeEvent<HTMLInputElement>) => {
    const files = e.target.files;
    if (!files || files.length === 0) return;
    setUploading(true);
    setUploadError(null);
    try {
      const results = await Promise.allSettled(
        Array.from(files).map((file) => api.upload(file)),
      );
      const uploaded: AgentMessageAttachment[] = [];
      const failed: string[] = [];
      for (let i = 0; i < results.length; i++) {
        const r = results[i];
        if (r.status === "fulfilled") {
          uploaded.push({ path: r.value.path, name: r.value.name, size: r.value.size, mime: r.value.mime });
        } else {
          failed.push(Array.from(files)[i].name);
        }
      }
      if (uploaded.length > 0) {
        setPendingFiles((prev) => [...prev, ...uploaded]);
      }
      if (failed.length > 0) {
        setUploadError(`Upload failed: ${failed.join(", ")}`);
      }
    } finally {
      setUploading(false);
      if (fileInputRef.current) fileInputRef.current.value = "";
    }
  }, []);

  const removePendingFile = useCallback((index: number) => {
    setPendingFiles((prev) => prev.filter((_, i) => i !== index));
  }, []);

  return {
    pendingFiles,
    setPendingFiles,
    uploading,
    uploadError,
    setUploadError,
    fileInputRef,
    handleFileSelect,
    removePendingFile,
  };
}
