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

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body className="min-h-screen antialiased">
        <Navbar />
        <div className="flex min-h-[calc(100vh-3.5rem)] flex-col">
          {children}
        </div>
      </body>
    </html>
  );
}
