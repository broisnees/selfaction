// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

const repo = 'https://github.com/broisnischal/selfaction';

export default defineConfig({
	integrations: [
		starlight({
			title: 'selfaction',
			description: 'Run GitHub Actions on machines you own: your laptop, a server or Docker.',
			social: [{ icon: 'github', label: 'GitHub', href: repo }],
			editLink: { baseUrl: `${repo}/edit/main/site/` },
			sidebar: [
				{ label: 'Overview', link: '/docs/' },
				{ label: 'Guides', items: [{ autogenerate: { directory: 'docs/guides' } }] },
				{ label: 'Concepts', items: [{ autogenerate: { directory: 'docs/concepts' } }] },
				{ label: 'Reference', items: [{ autogenerate: { directory: 'docs/reference' } }] },
			],
		}),
	],
});
