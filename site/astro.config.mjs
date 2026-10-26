// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import tailwindcss from '@tailwindcss/vite';

const repo = 'https://github.com/broisnischal/selfaction';

export default defineConfig({
	integrations: [
		starlight({
			title: 'selfaction',
			description: 'Run GitHub Actions on machines you own: your laptop, a server or Docker.',
			components: { SocialIcons: './src/components/HeaderLinks.astro' },
			social: [{ icon: 'github', label: 'GitHub', href: repo }],
			customCss: ['./src/styles/global.css'],
			editLink: { baseUrl: `${repo}/edit/main/site/` },
			sidebar: [
				{ label: 'Overview', link: '/docs/' },
				{ label: 'Guides', items: [{ autogenerate: { directory: 'docs/guides' } }] },
				{ label: 'Concepts', items: [{ autogenerate: { directory: 'docs/concepts' } }] },
				{ label: 'Reference', items: [{ autogenerate: { directory: 'docs/reference' } }] },
			],
		}),
	],
	vite: { plugins: [tailwindcss()] },
});
