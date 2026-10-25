import { useEffect, useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { authenticatedFetch } from "./api";
import { automation, uploadAttachment, type VoiceJob } from "./automation-api";
import { Button } from "./components/ui/button";
import { Notice } from "./App";

export function Voice({ conversationId, onDraft, reply = "" }: { conversationId: string; onDraft: (text: string) => void; reply?: string }) {
  const readiness = useQuery({ queryKey: ["voice-status"], queryFn: () => automation("voice.status", {}), gcTime: 0 });
  const [recording, setRecording] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [job, setJob] = useState<VoiceJob | null>(null);
  const [transcript, setTranscript] = useState("");
  const [audio, setAudio] = useState("");
  const recorder = useRef<MediaRecorder | null>(null);
  const stream = useRef<MediaStream | null>(null);
  const chunks = useRef<Blob[]>([]);
  const activeJob = useRef("");
  const alive = useRef(true);
  const audioURL = useRef("");
  const player = useRef<HTMLAudioElement>(null);
  const upload = useRef<AbortController | null>(null);
  const stopTimer = useRef<number | undefined>(undefined);
  const requestEpoch = useRef(0);
  function cleanup() {
    requestEpoch.current++;
    window.clearTimeout(stopTimer.current);
    if (recorder.current?.state === "recording") { recorder.current.onstop = null; recorder.current.stop(); }
    stream.current?.getTracks().forEach((track) => track.stop());
    stream.current = null; chunks.current = [];
    upload.current?.abort(); player.current?.pause();
    URL.revokeObjectURL(audioURL.current); audioURL.current = "";
    if (activeJob.current) { void automation("voice.cancel", { id: activeJob.current }).catch(() => {}); activeJob.current = ""; }
  }
  useEffect(() => {
    alive.current = true;
    setJob(null); setTranscript(""); setAudio(""); setRecording(false); setBusy(false);
    window.addEventListener("meshra:cleanup", cleanup);
    return () => { alive.current = false; cleanup(); window.removeEventListener("meshra:cleanup", cleanup); };
  }, [conversationId]);
  useEffect(() => {
    if (!job || !["pending", "running"].includes(job.state)) return;
    let disposed = false;
    const timer = window.setInterval(() => { void automation("voice.job", { id: job.id }).then((value) => { if (!disposed) setJob(value); }).catch((e: Error) => { if (!disposed) setError(e.message); }); }, 1000);
    return () => { disposed = true; window.clearInterval(timer); };
  }, [job?.id, job?.state]);
  useEffect(() => {
    if (!job) return;
    if (job.state === "completed" && job.transcript) setTranscript(job.transcript);
    if (job.error) setError(job.error);
    if (!job.audioAvailable || job.state !== "completed") return;
    const controller = new AbortController();
    void authenticatedFetch("voice/" + encodeURIComponent(job.id) + "/audio", { signal: controller.signal }).then(async (response) => {
      if (!response.ok) throw new Error("Local audio is unavailable (" + response.status + ").");
      const blob = await response.blob();
      if (controller.signal.aborted) return;
      URL.revokeObjectURL(audioURL.current); audioURL.current = URL.createObjectURL(blob); setAudio(audioURL.current);
    }).catch((e: Error) => { if (!controller.signal.aborted) setError(e.message); });
    return () => controller.abort();
  }, [job?.id, job?.state, job?.audioAvailable]);
  async function acceptJob(value: VoiceJob, epoch: number) {
    if (!alive.current || epoch !== requestEpoch.current) { await automation("voice.cancel", { id: value.id }).catch(() => {}); return; }
    activeJob.current = value.id; setJob(value);
  }
  async function record() {
    setError(""); setBusy(true);
    const epoch = ++requestEpoch.current;
    try {
      if (!navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === "undefined") throw new Error("Microphone recording is not supported in this browser. Use typed messages.");
      const media = await navigator.mediaDevices.getUserMedia({ audio: true });
      if (!alive.current || epoch !== requestEpoch.current) { media.getTracks().forEach((track) => track.stop()); return; }
      stream.current = media; chunks.current = [];
      const mimeType = ["audio/webm;codecs=opus", "audio/ogg;codecs=opus", "audio/mp4"].find((type) => MediaRecorder.isTypeSupported(type));
      const next = new MediaRecorder(media, mimeType ? { mimeType } : undefined); recorder.current = next;
      let bytes = 0;
      next.ondataavailable = (event) => { bytes += event.data.size; if (bytes > 10 * 1024 * 1024) { setError("Recording exceeds 10 MiB."); cleanup(); setRecording(false); return; } chunks.current.push(event.data); };
      next.onstop = () => {
        media.getTracks().forEach((track) => track.stop()); window.clearTimeout(stopTimer.current); setRecording(false);
        const blob = new Blob(chunks.current, { type: next.mimeType }); chunks.current = [];
        setBusy(true); upload.current = new AbortController();
        void uploadAttachment(blob, "microphone-recording", undefined, upload.current.signal).then((attachment) => automation("voice.transcribe", { audioAttachmentId: attachment.id })).then((value) => acceptJob(value, epoch)).catch((e: Error) => { if (alive.current && epoch === requestEpoch.current) setError(e.message); }).finally(() => { if (alive.current) setBusy(false); });
      };
      next.start(1000); setRecording(true);
      stopTimer.current = window.setTimeout(() => { if (next.state === "recording") next.stop(); }, 60000);
    } catch (e) { if (alive.current) setError((e as Error).message); }
    finally { if (alive.current) setBusy(false); }
  }
  async function speak() {
    setBusy(true); setError(""); const epoch = ++requestEpoch.current;
    try { await acceptJob(await automation("voice.synthesize", { text: reply, speakerId: readiness.data?.speakerId ?? 3 }), epoch); }
    catch (e) { if (alive.current) setError((e as Error).message); }
    finally { if (alive.current) setBusy(false); }
  }
  const pending = !!job && ["pending", "running"].includes(job.state);
  return <section className="automation-card"><strong>Local voice</strong><p className="muted">Microphone is off until you record. Transcripts are editable drafts; nothing is sent automatically. English/Chinese synthesis only.</p>
    {readiness.isError && <Notice>{readiness.error.message}</Notice>}{error && <Notice>{error}</Notice>}
    {readiness.data?.state !== "ready" && <p className="muted">Voice {readiness.data?.state || "readiness unavailable"}: {readiness.data?.prerequisites.join(" · ")}</p>}
    <div className="automation-actions"><Button type="button" disabled={busy || pending || (!recording && (readiness.data?.state !== "ready" || !readiness.data?.stt?.configured))} onClick={() => recording ? recorder.current?.stop() : void record()}>{recording ? "Stop recording" : "Record draft"}</Button><Button type="button" variant="outline" disabled={busy || pending || recording || !reply || new TextEncoder().encode(reply).length > 8192 || readiness.data?.state !== "ready" || !readiness.data?.tts?.configured} onClick={() => void speak()}>Read latest reply</Button><Button type="button" variant="ghost" onClick={() => { cleanup(); setRecording(false); setJob(null); setTranscript(""); setAudio(""); setBusy(false); }}>Cancel / clear voice</Button></div>
    {job && <p role="status">{job.kind}: {job.state}</p>}
    {transcript && <div className="form-stack"><label htmlFor="voice-transcript">Review transcript</label><textarea id="voice-transcript" value={transcript} onChange={(e) => setTranscript(e.target.value)} /><Button type="button" onClick={() => { onDraft(transcript); setTranscript(""); }}>Insert into composer</Button></div>}
    {audio && <audio ref={player} controls src={audio} />}
  </section>;
}
