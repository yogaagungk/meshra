import claude from "./assets/harnesses/claude.svg";
import omp from "./assets/harnesses/omp.svg";
import openai from "./assets/harnesses/openai.svg";
export const harnessNames: Record<string, string> = {
  claude: "Claude",
  omp: "OMP",
  openai: "Direct API",
};
const logos: Record<string, string> = { claude, omp, openai };
export function harnessName(id: string) {
  const h = id.split(":")[0];
  return harnessNames[h] || h;
}
export function HarnessLogo({ id }: { id: string }) {
  const h = id.split(":")[0];
  return (
    <span
      className={"harness harness-" + h}
      title={harnessName(id)}
      role="img"
      aria-label={harnessName(id)}
    >
      {logos[h] ? (
        <span
          className="harness-logo"
          style={{
            maskImage: `url("${logos[h]}")`,
            WebkitMaskImage: `url("${logos[h]}")`,
          }}
        />
      ) : (
        <span>{h.slice(0, 1).toUpperCase()}</span>
      )}
    </span>
  );
}

/** Department key for a harness or canonical agent id: claude, omp or openai (direct API). */
export function deptOf(id: string) {
  const h = id.split(":")[0];
  return h in logos ? h : "openai";
}
