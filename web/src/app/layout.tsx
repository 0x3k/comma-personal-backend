import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "comma personal",
  description:
    "Personal backend for collecting and reviewing dashcam videos, logs, and trip data from comma.ai devices",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en">
      <body className="antialiased">{children}</body>
    </html>
  );
}
