import Link from "next/link";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";

const NAV_CARDS: {
  href: string;
  title: string;
  badge: { label: string; variant: "neutral" | "info" | "success" };
  description: string;
}[] = [
  {
    href: "/routes",
    title: "Routes",
    badge: { label: "View all", variant: "info" },
    description:
      "Browse recorded driving routes, view dashcam footage, GPS tracks, and segment details.",
  },
  {
    href: "/devices",
    title: "Devices",
    badge: { label: "--", variant: "neutral" },
    description:
      "Manage registered comma devices and view their connection status.",
  },
  {
    href: "/settings",
    title: "Settings",
    badge: { label: "Configure", variant: "success" },
    description:
      "View and edit device parameters, check backend connectivity, and manage configuration.",
  },
];

export default function Home() {
  return (
    <PageWrapper
      title="Dashboard"
      description="Dashcam video, log, and trip data viewer"
    >
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {NAV_CARDS.map((card) => (
          <Link
            key={card.href}
            href={card.href}
            className="block rounded-lg transition-transform hover:scale-[1.01] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-[var(--ring-focus)]"
          >
            <Card className="h-full">
              <CardHeader>
                <div className="flex items-center justify-between">
                  <h2 className="text-subheading">{card.title}</h2>
                  <Badge variant={card.badge.variant}>{card.badge.label}</Badge>
                </div>
              </CardHeader>
              <CardBody>
                <p className="text-caption">{card.description}</p>
              </CardBody>
            </Card>
          </Link>
        ))}
      </div>
    </PageWrapper>
  );
}
