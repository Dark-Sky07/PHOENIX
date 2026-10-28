export type FilterPresetKind = 'domain' | 'ip';
export type FilterPresetGroup =
  | 'globalDomains'
  | 'youtube'
  | 'iranDomains'
  | 'russiaDomains'
  | 'privateIPs'
  | 'countryIPs';

export interface FilterPreset {
  key: string;
  kind: FilterPresetKind;
  group: FilterPresetGroup;
  value: string;
  caution?: 'broad' | 'large';
}

/**
 * Every preset below maps to a tag in the geosite/geoip files shipped by the
 * release workflow, or to a standard domain-list-community geosite tag.
 * Country-specific groups require the matching IR/RU data file.
 */
export const FILTER_PRESETS: FilterPreset[] = [
  { key: 'ads', kind: 'domain', group: 'globalDomains', value: 'geosite:category-ads' },
  { key: 'adsAll', kind: 'domain', group: 'globalDomains', value: 'geosite:category-ads-all' },
  { key: 'adsTelemetry', kind: 'domain', group: 'globalDomains', value: 'geosite:category-ads-and-telemetry' },
  { key: 'adult', kind: 'domain', group: 'globalDomains', value: 'geosite:category-porn', caution: 'broad' },
  { key: 'publicTrackers', kind: 'domain', group: 'globalDomains', value: 'geosite:category-public-tracker' },
  { key: 'socialMedia', kind: 'domain', group: 'globalDomains', value: 'geosite:category-social-media-!cn', caution: 'broad' },
  { key: 'socialMediaChina', kind: 'domain', group: 'globalDomains', value: 'geosite:category-social-media-cn', caution: 'broad' },
  { key: 'games', kind: 'domain', group: 'globalDomains', value: 'geosite:category-games', caution: 'broad' },
  { key: 'gamesOutsideChina', kind: 'domain', group: 'globalDomains', value: 'geosite:category-games-!cn', caution: 'broad' },
  { key: 'gamesChina', kind: 'domain', group: 'globalDomains', value: 'geosite:category-games-cn', caution: 'broad' },
  { key: 'cryptoServices', kind: 'domain', group: 'globalDomains', value: 'geosite:category-cryptocurrency', caution: 'broad' },
  { key: 'aiChat', kind: 'domain', group: 'globalDomains', value: 'geosite:category-ai-chat-!cn', caution: 'broad' },
  { key: 'vpnServices', kind: 'domain', group: 'globalDomains', value: 'geosite:category-vpnservices' },
  { key: 'telemetry', kind: 'domain', group: 'globalDomains', value: 'geosite:category-telemetry', caution: 'broad' },
  { key: 'communication', kind: 'domain', group: 'globalDomains', value: 'geosite:category-communication', caution: 'broad' },
  { key: 'voip', kind: 'domain', group: 'globalDomains', value: 'geosite:category-voip', caution: 'broad' },
  { key: 'developerTools', kind: 'domain', group: 'globalDomains', value: 'geosite:category-dev', caution: 'broad' },
  { key: 'entertainment', kind: 'domain', group: 'globalDomains', value: 'geosite:category-entertainment', caution: 'broad' },
  { key: 'bettingRussia', kind: 'domain', group: 'globalDomains', value: 'geosite:category-betting-ru', caution: 'broad' },
  { key: 'youtube', kind: 'domain', group: 'youtube', value: 'geosite:youtube', caution: 'broad' },

  { key: 'iranAds', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:ads' },
  { key: 'iranAdsAll', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:category-ads-all' },
  { key: 'iranMalware', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:malware' },
  { key: 'iranPhishing', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:phishing' },
  { key: 'iranCryptominers', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:cryptominers' },
  { key: 'iranSocial', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:social', caution: 'broad' },
  { key: 'iranAdultAndGambling', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:nsfw', caution: 'broad' },
  { key: 'iranSanctioned', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:sanctioned', caution: 'broad' },
  { key: 'iranAll', kind: 'domain', group: 'iranDomains', value: 'ext:geosite_IR.dat:ir', caution: 'broad' },

  { key: 'russiaAds', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:category-ads-all' },
  { key: 'russiaBlocked', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:ru-blocked' },
  { key: 'russiaBlockedAll', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:ru-blocked-all', caution: 'large' },
  { key: 'russiaAntiFilter', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:antifilter-download', caution: 'large' },
  { key: 'russiaCommunityBlocklist', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:antifilter-download-community' },
  { key: 'russiaReFilter', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:refilter' },
  { key: 'russiaInsideOnly', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:ru-available-only-inside', caution: 'broad' },
  { key: 'russiaWindowsSpy', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:win-spy' },
  { key: 'russiaWindowsUpdate', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:win-update', caution: 'broad' },
  { key: 'russiaWindowsExtra', kind: 'domain', group: 'russiaDomains', value: 'ext:geosite_RU.dat:win-extra', caution: 'broad' },

  { key: 'privateIPs', kind: 'ip', group: 'privateIPs', value: 'geoip:private', caution: 'broad' },
  { key: 'iranIPs', kind: 'ip', group: 'countryIPs', value: 'ext:geoip_IR.dat:ir' },
  { key: 'russiaIPs', kind: 'ip', group: 'countryIPs', value: 'ext:geoip_RU.dat:ru' },
  { key: 'chinaIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:cn' },
  { key: 'usIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:us' },
  { key: 'ukIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:gb' },
  { key: 'germanyIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:de' },
  { key: 'franceIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:fr' },
  { key: 'netherlandsIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:nl' },
  { key: 'vietnamIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:vn' },
  { key: 'spainIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:es' },
  { key: 'indonesiaIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:id' },
  { key: 'ukraineIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:ua' },
  { key: 'turkiyeIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:tr' },
  { key: 'brazilIPs', kind: 'ip', group: 'countryIPs', value: 'geoip:br' },
];

export const FILTER_PRESET_GROUPS: FilterPresetGroup[] = [
  'globalDomains',
  'youtube',
  'iranDomains',
  'russiaDomains',
  'privateIPs',
  'countryIPs',
];

export const DOMAIN_PRESET_VALUES = new Set(
  FILTER_PRESETS.filter((preset) => preset.kind === 'domain').map((preset) => preset.value),
);

export const IP_PRESET_VALUES = new Set(
  FILTER_PRESETS.filter((preset) => preset.kind === 'ip').map((preset) => preset.value),
);
