"use client";

import Link from "next/link";
import { useAlprSettings } from "@/lib/useAlprSettings";
import { AlprTuningCard } from "@/components/alpr/AlprTuningCard";
import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardBody } from "@/components/ui/Card";
import { Spinner } from "@/components/ui/Spinner";

/**
 * /settings/alpr/tuning is the operator-facing power-user surface for
 * the heuristic. The page is a thin wrapper around AlprTuningCard so
 * the card can be embedded elsewhere (e.g. a future "advanced
 * settings" tab) without re-implementing the layout.
 *
 * Like the alerts page, we gate rendering on the master alpr_enabled
 * flag so the surface does not light up when ALPR is off. The hook
 * caches across mounts so navigating from /settings -> /settings/alpr
 * /tuning is instant on the second visit.
 */
export default function AlprTuningPage() {
  const { enabled, loading, error } = useAlprSettings();

  return (
    <PageWrapper>
      <div className="mx-auto max-w-4xl space-y-4 p-4">
        <div>
          <Link
            href="/settings"
            className="text-sm text-[var(--accent)] hover:underline"
          >
            &larr; Back to settings
          </Link>
          <h1 className="mt-2 text-2xl font-semibold text-[var(--text-primary)]">
            ALPR Tuning
          </h1>
          <p className="text-sm text-[var(--text-secondary)]">
            Power-user controls for the stalking-detection heuristic. Most
            users should leave these at the defaults; nudge them only after
            you have observed false positives or missed alerts in your data.
          </p>
        </div>

        {loading && (
          <Card>
            <CardBody>
              <div className="flex items-center justify-center py-6">
                <Spinner label="Loading ALPR status" />
              </div>
            </CardBody>
          </Card>
        )}

        {!loading && (error || enabled !== true) && (
          <Card>
            <CardBody>
              <p className="text-sm text-[var(--text-secondary)]">
                {error
                  ? `Failed to load ALPR status: ${error}`
                  : "ALPR is currently disabled. Enable it from the main settings page before tuning."}
              </p>
            </CardBody>
          </Card>
        )}

        {!loading && enabled === true && <AlprTuningCard />}
      </div>
    </PageWrapper>
  );
}
