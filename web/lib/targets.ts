export type ConvertTarget = "go" | "py" | "cs";

export const CONVERT_TARGETS: {
  id: ConvertTarget;
  from: string;
  to: string;
  title: string;
  blurb: string;
  command: string;
  output: string;
}[] = [
  {
    id: "go",
    from: "Tuxedo .pc",
    to: "Go services",
    title: "Tux → Go",
    blurb: "Gin handlers + controllers + db stores with SQL-fidelity gates.",
    command: "convertgo",
    output: "pkg/services/<svc>/{handler,controller,db,models}",
  },
  {
    id: "py",
    from: "Batch .pc",
    to: "Python",
    title: "Tux → Python",
    blurb: "Batch programs → process_daily_batch modules with repo/DAL split.",
    command: "convertbatchpy",
    output: "<module>/{repo,service}.py + SQL constants",
  },
  {
    id: "cs",
    from: "Tuxedo .pc",
    to: "C# (.NET)",
    title: "Tux → C#",
    blurb: "Controller / DTO / NamedQueries / Repository / Service tree.",
    command: "convertcs",
    output: "Controller + DTO + NamedQueries + Repository + Service",
  },
];

export function targetOf(id: string): (typeof CONVERT_TARGETS)[number] {
  return CONVERT_TARGETS.find((t) => t.id === id) ?? CONVERT_TARGETS[0];
}
