import { z } from 'zod';

// SSTP is served by a local sstpd/pppd stack. The TLS material is deliberately
// per-inbound and never reused from the panel's web certificate.
export const SstpInboundSettingsSchema = z.object({
  localIP: z.string().default('10.254.0.1'),
  poolCIDR: z.string().default('10.254.0.0/24'),
  dns1: z.string().default('1.1.1.1'),
  dns2: z.string().default('8.8.8.8'),
  certificatePEM: z.string().default(''),
  privateKeyPEM: z.string().default(''),
  routeThroughXray: z.boolean().default(false),
  outboundTag: z.string().optional(),
  clients: z.array(z.any()).optional(),
});
export type SstpInboundSettings = z.infer<typeof SstpInboundSettingsSchema>;
