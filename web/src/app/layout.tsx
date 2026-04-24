import type { Metadata, Viewport } from "next";
import { Navbar } from "@/components/layout/Navbar";
import "./globals.css";

export const metadata: Metadata = {
  title: "comma personal",
  description:
    "Personal backend for collecting and reviewing dashcam videos, logs, and trip data from comma.ai devices",
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#fafafa" },
    { media: "(prefers-color-scheme: dark)", color: "#09090b" },
  ],
};

// Inline script injected into <head> to set the html theme class before the
// first paint, preventing a flash of incorrect theme. Kept deliberately
// small and dependency-free -- it runs as raw JS, not bundled React. Any
// change here must stay in sync with ThemeToggle.tsx (same storage key and
// same set of accepted values).
const themeInitScript = `(function(){try{var s=localStorage.getItem('theme');var p=(s==='light'||s==='dark'||s==='system')?s:'system';var d=p==='system'?window.matchMedia('(prefers-color-scheme: dark)').matches:p==='dark';var e=document.documentElement;e.classList.toggle('dark',d);e.classList.toggle('light',!d);}catch(_){}})();`;

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        <script
          // The string is a constant literal defined above; no user input
          // is concatenated, so the dangerouslySetInnerHTML here is safe.
          dangerouslySetInnerHTML={{ __html: themeInitScript }}
        />
      </head>
      <body className="min-h-screen antialiased">
        <Navbar />
        <div className="flex min-h-[calc(100vh-3.5rem)] flex-col">
          {children}
        </div>
      </body>
    </html>
  );
}
