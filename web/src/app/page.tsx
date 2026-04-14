import { PageWrapper } from "@/components/layout/PageWrapper";
import { Card, CardHeader, CardBody } from "@/components/ui/Card";
import { Badge } from "@/components/ui/Badge";
import { Spinner } from "@/components/ui/Spinner";

export default function Home() {
  return (
    <PageWrapper
      title="Dashboard"
      description="Dashcam video, log, and trip data viewer"
    >
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Card>
          <CardHeader>
            <div className="flex items-center justify-between">
              <h2 className="text-subheading">Devices</h2>
              <Badge variant="neutral">--</Badge>
            </div>
          </CardHeader>
          <CardBody>
            <p className="text-caption">
              No devices connected yet. Pair a comma device to get started.
            </p>
          </CardBody>
        </Card>

        <Card>
          <CardHeader>
            <div className="flex items-center justify-between">
              <h2 className="text-subheading">Recent Routes</h2>
              <Badge variant="info">0</Badge>
            </div>
          </CardHeader>
          <CardBody>
            <p className="text-caption">
              Routes will appear here once your device uploads data.
            </p>
          </CardBody>
        </Card>

        <Card>
          <CardHeader>
            <div className="flex items-center justify-between">
              <h2 className="text-subheading">Storage</h2>
              <Badge variant="success">Healthy</Badge>
            </div>
          </CardHeader>
          <CardBody>
            <p className="text-caption">
              Local storage for video, log, and trip data.
            </p>
          </CardBody>
        </Card>
      </div>

      {/* Loading state demo */}
      <div className="mt-8">
        <Card>
          <CardBody>
            <div className="flex items-center justify-center py-8">
              <Spinner label="Waiting for device data" />
            </div>
          </CardBody>
        </Card>
      </div>
    </PageWrapper>
  );
}
