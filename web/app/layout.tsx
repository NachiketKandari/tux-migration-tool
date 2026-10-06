import type { Metadata } from "next";
import "./globals.css";
import { TooltipProvider } from "@/components/ui/tooltip";

export const metadata: Metadata = {
  title: "tux-migration-tool — Tuxedo Pro*C to Go, Python, C#",
  description:
    "Drop a .pc file — visualize the IR breakdown, explore dispatch-axis scenarios, tag mappings, and convert to Go, Python, or C#.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        {/* Set the theme class before paint: stored pref wins, else OS setting. */}
        <script
          dangerouslySetInnerHTML={{
            __html: `(function(){try{var k="tux-theme";var s=localStorage.getItem(k);var d=s==="dark"||(!s&&matchMedia("(prefers-color-scheme: dark)").matches);if(d)document.documentElement.classList.add("dark")}catch(e){}})();`,
          }}
        />
      </head>
      <body className="min-h-screen bg-background antialiased">
        <TooltipProvider delayDuration={200}>{children}</TooltipProvider>
      </body>
    </html>
  );
}
