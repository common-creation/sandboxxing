import * as fs from 'fs';
import * as path from 'path';
import {
  Aspects,
  Aws,
  CfnOutput,
  CfnResource,
  CustomResource,
  Duration,
  IAspect,
  RemovalPolicy,
  Stack,
  StackProps,
} from 'aws-cdk-lib';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as efs from 'aws-cdk-lib/aws-efs';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as lambda from 'aws-cdk-lib/aws-lambda';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as s3assets from 'aws-cdk-lib/aws-s3-assets';
import * as cr from 'aws-cdk-lib/custom-resources';
import { Construct, IConstruct } from 'constructs';

// The IAM path of the execution roles that the daemon creates, one per
// container. It must match microvm.role_path of the daemon configuration.
const ROLE_PATH = '/sandboxxing/instances/';

// The ports of the agent: the data port is forwarded by the MicroVM endpoint,
// the hook port is only used by Lambda inside the VM.
const HOOK_PORT = 9000;

// Lambda gives each runtime hook at most a minute.
const HOOK_TIMEOUT = 60;

/** Every resource of the stack is deleted with it, including the data. */
class DestroyEverything implements IAspect {
  visit(node: IConstruct): void {
    if (node instanceof CfnResource) {
      node.applyRemovalPolicy(RemovalPolicy.DESTROY);
    }
  }
}

/**
 * The AWS resources of the sandboxxing microvm backend: the network the
 * MicroVMs reach the home directories through, the EFS file system of the
 * home directories, the MicroVM image with the agent, and the policies of the
 * daemon and of the execution roles it creates.
 *
 * Context values:
 *   baseImageVersion  version of the Lambda managed base image (required), see
 *                     `aws lambda-microvms list-managed-microvm-image-versions`
 *   baseImageArn      managed base image (default: al2023-1 of the region)
 *   imageName         name of the MicroVM image (default: sandboxxing)
 *   memoryMiB         baseline memory of a MicroVM (default: 2048)
 *   natGateways       NAT gateways for internet access over the VPC (default: 1)
 */
export class SandboxxingMicroVMStack extends Stack {
  constructor(scope: Construct, id: string, props?: StackProps) {
    super(scope, id, props);
    Aspects.of(this).add(new DestroyEverything());

    const baseImageVersion = this.node.tryGetContext('baseImageVersion');
    if (!baseImageVersion) {
      throw new Error(
        'pass the version of the managed base image: cdk deploy -c baseImageVersion=<version> ' +
          '(aws lambda-microvms list-managed-microvm-image-versions --image-identifier ' +
          'arn:aws:lambda:<region>:aws:microvm-image:al2023-1)',
      );
    }
    const baseImageArn: string =
      this.node.tryGetContext('baseImageArn') ?? `arn:${Aws.PARTITION}:lambda:${Aws.REGION}:aws:microvm-image:al2023-1`;
    const imageName: string = this.node.tryGetContext('imageName') ?? 'sandboxxing';
    const memoryMiB = Number(this.node.tryGetContext('memoryMiB') ?? 2048);
    const natGateways = Number(this.node.tryGetContext('natGateways') ?? 1);

    const imageDir = path.join(__dirname, '..', '..', 'microvm-image');
    if (!fs.existsSync(path.join(imageDir, 'sandboxxing-agent'))) {
      throw new Error(`${imageDir}/sandboxxing-agent is missing: run "make agent" first`);
    }

    // The network. The MicroVMs reach the mount targets of the file system
    // through a VPC egress connector; the NAT gateways give them internet
    // access on the same path.
    const vpc = new ec2.Vpc(this, 'Vpc', {
      maxAzs: 2,
      natGateways,
      subnetConfiguration: [
        { name: 'public', subnetType: ec2.SubnetType.PUBLIC },
        {
          name: 'private',
          subnetType: natGateways > 0 ? ec2.SubnetType.PRIVATE_WITH_EGRESS : ec2.SubnetType.PRIVATE_ISOLATED,
        },
      ],
    });
    const privateSubnets = vpc.selectSubnets({ subnetGroupName: 'private' });
    const microvmSg = new ec2.SecurityGroup(this, 'MicroVMSecurityGroup', {
      vpc,
      description: 'sandboxxing MicroVMs',
      allowAllOutbound: true,
    });
    const efsSg = new ec2.SecurityGroup(this, 'FileSystemSecurityGroup', {
      vpc,
      description: 'sandboxxing home directories: NFS from the MicroVMs only',
      allowAllOutbound: false,
    });
    efsSg.addIngressRule(microvmSg, ec2.Port.tcp(2049), 'NFS from the MicroVMs');

    // The home directories. Each container gets an access point, created by
    // the daemon, and an execution role that may mount only that access
    // point. The file system policy below adds the rules that apply to every
    // client: TLS, and no access that bypasses an access point.
    //
    // The policy deliberately contains no Allow statement. A file system
    // policy that exists denies anonymous clients, and any Allow here would
    // be granted on top of the narrow identity policies of the roles.
    // allowAnonymousAccess only stops the construct from adding its own
    // statement, which grants write and root access to any principal; it does
    // not allow anonymous access while this policy is in place.
    const fileSystemPolicy = new iam.PolicyDocument({
      statements: [
        new iam.PolicyStatement({
          sid: 'DenyWithoutTLS',
          effect: iam.Effect.DENY,
          principals: [new iam.AnyPrincipal()],
          actions: ['elasticfilesystem:*'],
          resources: ['*'],
          conditions: { Bool: { 'aws:SecureTransport': 'false' } },
        }),
        new iam.PolicyStatement({
          sid: 'DenyWithoutAccessPoint',
          effect: iam.Effect.DENY,
          principals: [new iam.AnyPrincipal()],
          actions: ['elasticfilesystem:ClientMount', 'elasticfilesystem:ClientWrite', 'elasticfilesystem:ClientRootAccess'],
          resources: ['*'],
          conditions: { Null: { 'elasticfilesystem:AccessPointArn': 'true' } },
        }),
      ],
    });
    const fileSystem = new efs.FileSystem(this, 'Homes', {
      vpc,
      vpcSubnets: privateSubnets,
      securityGroup: efsSg,
      encrypted: true,
      performanceMode: efs.PerformanceMode.GENERAL_PURPOSE,
      throughputMode: efs.ThroughputMode.ELASTIC,
      removalPolicy: RemovalPolicy.DESTROY,
      allowAnonymousAccess: true,
      fileSystemPolicy,
    });

    const logGroup = new logs.LogGroup(this, 'Logs', {
      logGroupName: '/sandboxxing/microvms',
      retention: logs.RetentionDays.ONE_MONTH,
      removalPolicy: RemovalPolicy.DESTROY,
    });

    // The upper bound of every execution role the daemon creates. The inline
    // policy of a role narrows the mount permission down to its access point.
    const boundary = new iam.ManagedPolicy(this, 'ExecutionRoleBoundary', {
      description: 'Upper bound of the execution roles of sandboxxing containers',
      statements: [
        new iam.PolicyStatement({
          actions: ['elasticfilesystem:ClientMount', 'elasticfilesystem:ClientWrite', 'elasticfilesystem:ClientRootAccess'],
          resources: [fileSystem.fileSystemArn],
          conditions: { Bool: { 'elasticfilesystem:AccessedViaMountTarget': 'true' } },
        }),
        new iam.PolicyStatement({
          actions: ['logs:CreateLogStream', 'logs:PutLogEvents'],
          // logGroupArn ends with :*, which covers the log streams.
          resources: [logGroup.logGroupArn],
        }),
      ],
    });

    // The network connector manages ENIs in the private subnets.
    const operatorRole = new iam.Role(this, 'NetworkConnectorOperator', {
      assumedBy: new iam.ServicePrincipal('lambda.amazonaws.com'),
      description: 'Lets Lambda create the ENIs of the sandboxxing network connector',
    });
    operatorRole.addToPolicy(
      new iam.PolicyStatement({
        actions: ['ec2:CreateNetworkInterface'],
        resources: [
          `arn:${Aws.PARTITION}:ec2:*:*:network-interface/*`,
          `arn:${Aws.PARTITION}:ec2:*:*:subnet/*`,
          `arn:${Aws.PARTITION}:ec2:*:*:security-group/*`,
        ],
      }),
    );
    operatorRole.addToPolicy(
      new iam.PolicyStatement({
        actions: ['ec2:CreateTags'],
        resources: [`arn:${Aws.PARTITION}:ec2:*:*:network-interface/*`],
        conditions: { StringEquals: { 'ec2:ManagedResourceOperator': 'network-connectors.lambda.amazonaws.com' } },
      }),
    );
    operatorRole.addToPolicy(
      new iam.PolicyStatement({
        actions: [
          'ec2:DeleteNetworkInterface',
          'ec2:DescribeNetworkInterfaces',
          'ec2:DescribeSecurityGroups',
          'ec2:DescribeSubnets',
          'ec2:DescribeVpcs',
        ],
        resources: ['*'],
      }),
    );
    const connector = new lambda.CfnNetworkConnector(this, 'NetworkConnector', {
      name: 'sandboxxing',
      operatorRole: operatorRole.roleArn,
      configuration: {
        vpcEgressConfiguration: {
          subnetIds: privateSubnets.subnetIds,
          securityGroupIds: [microvmSg.securityGroupId],
          networkProtocol: 'IPv4',
          associatedComputeResourceTypes: ['MicroVm'],
        },
      },
    });
    connector.node.addDependency(operatorRole);

    // The MicroVM image: the Dockerfile and the agent binary.
    const code = new s3assets.Asset(this, 'ImageCode', { path: imageDir });
    const buildRole = new iam.Role(this, 'ImageBuildRole', {
      assumedBy: new iam.ServicePrincipal('lambda.amazonaws.com').withSessionTags(),
      description: 'Builds the sandboxxing MicroVM image',
    });
    code.grantRead(buildRole);
    logGroup.grantWrite(buildRole);
    const image = new lambda.CfnMicrovmImage(this, 'Image', {
      name: imageName,
      description: 'sandboxxing agent on Amazon Linux 2023',
      baseImageArn,
      baseImageVersion,
      buildRoleArn: buildRole.roleArn,
      codeArtifact: { uri: code.s3ObjectUrl },
      cpuConfigurations: [{ architecture: 'ARM_64' }],
      resources: [{ minimumMemoryInMiB: memoryMiB }],
      // The agent mounts the home directory, which needs the capabilities.
      additionalOsCapabilities: ['ALL'],
      // The build reaches the package repositories over the internet, which
      // is also what the API defaults to; the VPC connector is chosen per
      // MicroVM by the daemon.
      egressNetworkConnectors: [
        `arn:${Aws.PARTITION}:lambda:${Aws.REGION}:aws:network-connector:aws-network-connector:INTERNET_EGRESS`,
      ],
      environmentVariables: [],
      hooks: {
        port: HOOK_PORT,
        microvmHooks: {
          run: 'ENABLED',
          runTimeoutInSeconds: HOOK_TIMEOUT,
          resume: 'ENABLED',
          resumeTimeoutInSeconds: HOOK_TIMEOUT,
          suspend: 'ENABLED',
          suspendTimeoutInSeconds: 30,
          terminate: 'ENABLED',
          terminateTimeoutInSeconds: HOOK_TIMEOUT,
        },
        microvmImageHooks: {
          ready: 'ENABLED',
          readyTimeoutInSeconds: 300,
        },
      },
      logging: { cloudWatch: { logGroup: logGroup.logGroupName } },
    });
    image.node.addDependency(buildRole);

    // The permissions of the daemon. Attach the policy to the identity the
    // daemon runs with.
    const instanceRoles = `arn:${Aws.PARTITION}:iam::${Aws.ACCOUNT_ID}:role${ROLE_PATH}*`;
    const daemonPolicy = new iam.ManagedPolicy(this, 'DaemonPolicy', {
      description: 'Permissions of the sandboxxing daemon for the microvm backend',
      statements: [
        new iam.PolicyStatement({
          sid: 'RunMicroVMs',
          actions: ['lambda:RunMicrovm'],
          resources: [image.attrImageArn, `${image.attrImageArn}:*`],
        }),
        // Running a MicroVM with a connector requires passing it, which the
        // MicroVM documentation does not mention.
        new iam.PolicyStatement({
          sid: 'PassNetworkConnectors',
          actions: ['lambda:PassNetworkConnector'],
          resources: [
            connector.attrArn,
            `${connector.attrArn}:*`,
            `arn:${Aws.PARTITION}:lambda:${Aws.REGION}:aws:network-connector:*`,
          ],
        }),
        new iam.PolicyStatement({
          sid: 'ManageMicroVMs',
          actions: [
            'lambda:GetMicrovm',
            'lambda:ListMicrovms',
            'lambda:ResumeMicrovm',
            'lambda:TerminateMicrovm',
            'lambda:CreateMicrovmAuthToken',
          ],
          resources: ['*'],
        }),
        new iam.PolicyStatement({
          sid: 'CreateHomeDirectories',
          actions: ['elasticfilesystem:CreateAccessPoint'],
          resources: [fileSystem.fileSystemArn],
        }),
        new iam.PolicyStatement({
          sid: 'TagHomeDirectories',
          actions: ['elasticfilesystem:TagResource'],
          // Tags passed to CreateAccessPoint are authorised against the file
          // system, since the access point does not exist yet.
          resources: [
            fileSystem.fileSystemArn,
            `arn:${Aws.PARTITION}:elasticfilesystem:${Aws.REGION}:${Aws.ACCOUNT_ID}:access-point/*`,
          ],
        }),
        new iam.PolicyStatement({
          sid: 'DescribeHomeDirectories',
          actions: ['elasticfilesystem:DescribeAccessPoints'],
          resources: ['*'],
        }),
        new iam.PolicyStatement({
          sid: 'DeleteHomeDirectories',
          actions: ['elasticfilesystem:DeleteAccessPoint'],
          resources: [`arn:${Aws.PARTITION}:elasticfilesystem:${Aws.REGION}:${Aws.ACCOUNT_ID}:access-point/*`],
          conditions: { Null: { 'aws:ResourceTag/sandboxxing:instance': 'false' } },
        }),
        // The execution roles can only be created with the boundary, so the
        // daemon cannot hand out more than mounting a home directory.
        new iam.PolicyStatement({
          sid: 'CreateExecutionRoles',
          actions: ['iam:CreateRole', 'iam:PutRolePolicy'],
          resources: [instanceRoles],
          conditions: { StringEquals: { 'iam:PermissionsBoundary': boundary.managedPolicyArn } },
        }),
        new iam.PolicyStatement({
          sid: 'DeleteExecutionRoles',
          actions: ['iam:TagRole', 'iam:DeleteRolePolicy', 'iam:DeleteRole'],
          resources: [instanceRoles],
        }),
        new iam.PolicyStatement({
          sid: 'PassExecutionRoles',
          actions: ['iam:PassRole'],
          resources: [instanceRoles],
          conditions: { StringEquals: { 'iam:PassedToService': 'lambda.amazonaws.com' } },
        }),
      ],
    });

    // The access points and execution roles are created by the daemon, so
    // they are not part of the stack. They are removed before the file
    // system when the stack is deleted, together with the MicroVMs that
    // still run, which hold ENIs of the network connector.
    // The functions log to groups of the stack, as the groups Lambda creates
    // by itself would outlive it.
    const cleanupLogs = new logs.LogGroup(this, 'CleanupLogs', {
      retention: logs.RetentionDays.ONE_WEEK,
      removalPolicy: RemovalPolicy.DESTROY,
    });
    const cleanupFn = new lambda.Function(this, 'CleanupFunction', {
      runtime: lambda.Runtime.NODEJS_22_X,
      handler: 'index.handler',
      timeout: Duration.minutes(10),
      logGroup: cleanupLogs,
      code: lambda.Code.fromInline(fs.readFileSync(path.join(__dirname, '..', 'cleanup', 'index.js'), 'utf8')),
      environment: {
        FILE_SYSTEM_ID: fileSystem.fileSystemId,
        ROLE_PATH,
        IMAGE_ARN: image.attrImageArn,
      },
    });
    cleanupFn.addToRolePolicy(
      new iam.PolicyStatement({
        actions: ['elasticfilesystem:DescribeAccessPoints', 'elasticfilesystem:DeleteAccessPoint'],
        resources: ['*'],
      }),
    );
    cleanupFn.addToRolePolicy(
      new iam.PolicyStatement({
        actions: ['iam:ListRoles', 'iam:ListRolePolicies', 'iam:DeleteRolePolicy', 'iam:DeleteRole'],
        resources: ['*'],
      }),
    );
    cleanupFn.addToRolePolicy(
      new iam.PolicyStatement({
        actions: ['lambda:ListMicrovms', 'lambda:GetMicrovm', 'lambda:TerminateMicrovm'],
        resources: ['*'],
      }),
    );
    const provider = new cr.Provider(this, 'CleanupProvider', { onEventHandler: cleanupFn, logGroup: cleanupLogs });
    const cleanup = new CustomResource(this, 'Cleanup', { serviceToken: provider.serviceToken });
    // A resource is deleted before the ones it depends on.
    cleanup.node.addDependency(fileSystem, connector, image, boundary);

    new CfnOutput(this, 'ImageArn', { value: image.attrImageArn });
    new CfnOutput(this, 'NetworkConnectorArn', { value: connector.attrArn });
    new CfnOutput(this, 'FileSystemId', { value: fileSystem.fileSystemId });
    new CfnOutput(this, 'PermissionsBoundaryArn', { value: boundary.managedPolicyArn });
    new CfnOutput(this, 'DaemonPolicyArn', {
      value: daemonPolicy.managedPolicyArn,
      description: 'Attach to the IAM identity of the sandboxxing daemon',
    });
    new CfnOutput(this, 'DaemonConfig', {
      description: 'The microvm section of /etc/sandboxxing/config.json',
      value: this.toJsonString({
        microvm: {
          region: Aws.REGION,
          images: { al2023: { arn: image.attrImageArn } },
          egress_network_connectors: [connector.attrArn],
          efs_file_system_id: fileSystem.fileSystemId,
          role_path: ROLE_PATH,
          permissions_boundary_arn: boundary.managedPolicyArn,
          log_group: logGroup.logGroupName,
        },
      }),
    });
  }
}
