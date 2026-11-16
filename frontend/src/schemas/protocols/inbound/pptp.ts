import { z } from 'zod';

// PPTP is retained only for legacy compatibility. Its MPPE/MS-CHAPv2 security
// is obsolete; deployments should prefer SSTP or another modern tunnel.
export const PptpInboundSettingsSchema = z.object({
  localIP: z.string().default('10.253.0.1'),
  poolCIDR: z.string().default('10.253.0.0/24'),
  dns1: z.string().default('1.1.1.1'),
  dns2: z.string().default('8.8.8.8'),
  routeThroughXray: z.boolean().default(false),
  outboundTag: z.string().optional(),
  clients: z.array(z.any()).optional(),
});
export type PptpInboundSettings = z.infer<typeof PptpInboundSettingsSchema>;
