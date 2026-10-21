import codex from "./assets/harnesses/codex.svg";
import claude from "./assets/harnesses/claude.svg";
import opencode from "./assets/harnesses/opencode.svg";
import omp from "./assets/harnesses/omp.svg";
export const harnessNames: Record<string, string> = {
  codex: "Codex",
  claude: "Claude",
  opencode: "OpenCode",
  omp: "OMP",
};
const logos: Record<string, string> = { codex, claude, opencode, omp };
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
