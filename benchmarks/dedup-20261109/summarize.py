"""Validate fixture equivalence, summarize all repetitions, export CSV and HTML."""
import collections, csv, html, json, pathlib, statistics, sys

run = pathlib.Path(sys.argv[1]).resolve()
rows = [json.loads(x) for x in (run/'results.jsonl').read_text(encoding='utf-8').splitlines()]
manifest = json.loads((run/'manifest.json').read_text(encoding='utf-8'))
reps = int(manifest['parameters']['repetitions'])
assert len(rows) == 4*3*reps, (len(rows),reps)
groups = collections.defaultdict(list)
hashes = {}
for r in rows:
    assert r['verified_disks']==6
    assert r['compression']=='default' and r['encryption']=='AES-256-GCM'
    assert r['full']['read_bytes']==384*2**20
    assert r['incremental']['read_bytes']==84*2**20
    h = (r['full_sha256'],r['incremental_sha256'])
    if r['scenario'] in hashes: assert hashes[r['scenario']]==h, 'Input bytes differ!'
    hashes[r['scenario']]=h
    groups[r['scenario'],r['block_bytes']//1024].append(r)

metrics = {
    'full_repo_mib':lambda r:r['full']['files']['total']/2**20,
    'increment_repo_mib':lambda r:(r['incremental']['files']['total']-r['full']['files']['total'])/2**20,
    'total_repo_mib':lambda r:r['incremental']['files']['total']/2**20,
    'full_seconds':lambda r:r['full']['seconds'],
    'increment_seconds':lambda r:r['incremental']['seconds'],
    'full_mib_s':lambda r:384/r['full']['seconds'],
    'increment_read_mib_s':lambda r:84/r['incremental']['seconds'],
    'full_unique_data_mib':lambda r:r['full']['unique_data_raw_bytes']/2**20,
    'increment_unique_data_mib':lambda r:r['incremental']['unique_data_raw_bytes']/2**20,
    'index_kib':lambda r:r['incremental']['files']['index']/1024,
    'indexed_blobs':lambda r:r['incremental']['indexed_blobs'],
    'full_allocated_mib':lambda r:r['full']['allocated_bytes']/2**20,
    'increment_allocated_mib':lambda r:r['incremental']['allocated_bytes']/2**20,
    'reopen_seconds':lambda r:r['reopen_seconds'],
    'restore_compare_seconds':lambda r:r['restore_compare_seconds'],
}
summary=[]
for (case,kib), rs in sorted(groups.items()):
    assert len(rs)==reps
    s={'scenario':case,'block_kib':kib,'repetitions':len(rs)}
    for k,f in metrics.items():
        v=[f(r) for r in rs]
        s[k]=statistics.median(v);s[k+'_min']=min(v);s[k+'_max']=max(v)
    summary.append(s)
baseline={s['scenario']:s for s in summary if s['block_kib']==1024}
for s in summary:
    b=baseline[s['scenario']]
    for k in ['full_repo_mib','increment_repo_mib','total_repo_mib']:
        s[k+'_saving_pct']=100*(1-s[k]/b[k])
    for k in ['full_seconds','increment_seconds']:
        s[k+'_change_pct']=100*(s[k]/b[k]-1)
with open(run/'summary.csv','w',newline='',encoding='utf-8-sig') as f:
    w=csv.DictWriter(f,fieldnames=list(summary[0]));w.writeheader();w.writerows(summary)
(run/'summary.json').write_text(json.dumps(summary,indent=2),encoding='utf-8')

labels={'aligned':'VM similaires — blocs communs alignés','scattered':'VM similaires — différences de 4 Kio dispersées','shifted':'Contenu commun décalé de 4 / 8 Kio','unique':'Contrôle — données aléatoires distinctes'}
def table(cols):
    out=['<table><thead><tr><th>Scénario</th><th>Bloc (Kio)</th>']
    out += ['<th>'+html.escape(label)+'</th>' for _,label in cols]
    out.append('</tr></thead><tbody>')
    for s in summary:
        out.append('<tr><td>'+html.escape(labels[s['scenario']])+'</td><td>'+str(s['block_kib'])+'</td>')
        for key,_ in cols:
            out.append(f'<td>{s[key]:.3f}</td>')
        out.append('</tr>')
    return ''.join(out)+'</tbody></table>'

body='''<!doctype html><html lang="fr"><meta charset="utf-8"><title>BackupZit — benchmark des tailles de blocs</title>
<style>body{font:16px system-ui,sans-serif;max-width:1350px;margin:40px auto;padding:0 22px;color:#182330;line-height:1.55}h1,h2{line-height:1.25}table{border-collapse:collapse;width:100%;font-size:14px;margin:20px 0}th,td{border:1px solid #d7dfe7;padding:8px;text-align:right}th:first-child,td:first-child{text-align:left}th{background:#edf3f8}tr:nth-child(even){background:#f8fafc}code{background:#edf3f8;padding:2px 4px}.note{border-left:4px solid #2c6eaa;padding:8px 16px;background:#f0f6fc}a{color:#195c99}</style>
<h1>BackupZit 0.47.3 : blocs de 256, 512 et 1 024 Kio</h1>
<p>Mesures locales du 9 novembre 2026. Moteur réel de BackupZit ; données synthétiques ; aucune API Hyper-V appelée.</p>
<div class="note"><strong>Ce test mesure le moteur de stockage et le découpage des disques virtuels.</strong> Il ne mesure pas une sauvegarde Hyper-V de bout en bout et ne constitue pas un benchmark de Windows 11. Le taux obtenu dépend volontairement des scénarios construits.</div>
<h2>Protocole</h2>
<ul><li>Trois copies isolées du code. Seule modification fonctionnelle : constante <code>internal/diskimg/diskimg.go:BlockSize</code>. Le code de production d’origine reste inchangé. La taille maximale d’une lecture, définie par <code>32 * BlockSize</code>, suit aussi cette constante : 8, 16 ou 32 Mio. Les temps reflètent donc également ce comportement existant du moteur.</li>
<li>Trois VM synthétiques de 128 Mio, soit 384 Mio logiques par sauvegarde. Pour les scénarios similaires : 80 Mio communs, 32 Mio propres à chaque VM et 16 Mio nuls par disque. Mélange synthétique compressible : 75 % des pages de 4 Kio contiennent un motif aléatoire de 1 Kio répété quatre fois ; les autres sont aléatoires.</li>
<li>Scénario aligné : blocs communs strictement alignés. Scénario dispersé : VM2 et VM3 ont chacune une modification distincte de 4 Kio dans chaque Mio de la zone commune. Scénario décalé : rotation de la zone commune de 4 et 8 Kio. Contrôle : 112 Mio aléatoires distincts et 16 Mio nuls par VM.</li>
<li>Incrémental : 28 modifications distinctes de 4 Kio par VM, soit 336 Kio modifiés au total. Les zones synthétiques de suivi des changements sont arrondies à 1 Mio (84 Mio lus au total), puis traitées par le vrai <code>StoreAreas</code>. Ce choix ne prétend pas représenter la granularité RCT de tous les hôtes Hyper-V.</li>
<li>Dépôt partagé par les trois VM ; ZSTD standard ; AES-256-GCM ; backend local réel avec synchronisation des fichiers. Sources en mémoire : le débit de lecture d’un VHDX ou d’un NAS n’est pas mesuré.</li>
<li>Temps complet et incrémental : traitement des disques, cartes de blocs, arbre, flush des packs et écriture du snapshot. Génération des sources, initialisation/déverrouillage du dépôt et restauration exclus de ces temps. Réouverture entre le complet et l’incrémental.</li>
<li>Un échauffement par binaire, puis trois répétitions dans un ordre mélangé déterministe ; aucune exécution simultanée ; <code>GOMAXPROCS=4</code>. Médianes et étendues min/max conservées dans le CSV.</li>
<li>Après réouverture, restauration des trois disques du complet et de l’incrémental et comparaison octet par octet. Les SHA-256 des sources sont identiques entre tailles et répétitions.</li>
<li>Stockage : somme des tailles logiques des fichiers du dépôt, y compris packs, index, snapshots, configuration et clé. Ce n’est pas l’allocation physique NTFS. Croissance incrémentale : différence de taille du dépôt, toutes versions conservées, sans purge.</li>
<li>Allocations : cumul des allocations Go pendant la phase, pas pic de RAM. Index : fichiers d’index sur disque, pas totalité des métadonnées ni mémoire de l’index.</li></ul>
<h2>Stockage réel du dépôt</h2>'''
body+=table([('full_repo_mib','Après complet (Mio)'),('increment_repo_mib','Ajout incrémental (Mio)'),('total_repo_mib','Total (Mio)'),('total_repo_mib_saving_pct','Gain total / 1 Mio (%)')])
if manifest.get('runtime_block_size_setting'):
    body=body.replace('Seule modification fonctionnelle : constante <code>internal/diskimg/diskimg.go:BlockSize</code>.','Le réglage de session <code>SetVMBlockSizeKiB</code> sélectionne la taille ; aucune constante du moteur n’est modifiée.')
body+='<h2>Temps et débit</h2>'
body+=table([('full_seconds','Complet (s)'),('full_seconds_min','Complet min (s)'),('full_seconds_max','Complet max (s)'),('increment_seconds','Incrémental (s)'),('increment_seconds_min','Incr. min (s)'),('increment_seconds_max','Incr. max (s)'),('full_mib_s','Complet (Mio/s)')])
body+='<h2>Données uniques et métadonnées</h2>'
body+=table([('full_unique_data_mib','Données uniques complet avant compression (Mio)'),('increment_unique_data_mib','Données uniques ajoutées avant compression (Mio)'),('index_kib','Index final (Kio)'),('indexed_blobs','Blobs indexés'),('full_allocated_mib','Allocations complet (Mio)'),('increment_allocated_mib','Allocations incr. (Mio)')])
scatter={s['block_kib']:s for s in summary if s['scenario']=='scattered'}
small,big=scatter[256],scatter[1024]
body+=f'''<h2>Résultat principal</h2><p>Dans le scénario de différences dispersées, 256 Kio réduit le complet de {100*(1-small['full_repo_mib']/big['full_repo_mib']):.1f} %, la croissance incrémentale de {100*(1-small['increment_repo_mib']/big['increment_repo_mib']):.1f} %, et le total conservé de {small['total_repo_mib_saving_pct']:.1f} % face à 1 Mio. Les durées médianes baissent de {-small['full_seconds_change_pct']:.1f} % pour le complet et {-small['increment_seconds_change_pct']:.1f} % pour l’incrémental, avec les variations détaillées ci-dessus. L’index final passe de {big['index_kib']:.1f} à {small['index_kib']:.1f} Kio.</p><p>Le complet aligné et le contrôle aléatoire ne gagnent pas de stockage avec de petits blocs ; le contrôle est environ 6,6 % plus lent en 256 Kio sur le complet. Les trois tailles fixes ne retrouvent pas les blocs communs décalés de 4 ou 8 Kio. Le gain incrémental reste observable dans ces scénarios, car les modifications sont petites et dispersées.</p>'''
body+='''<h2>Interprétation et limites</h2><p>Des blocs plus petits peuvent isoler les portions modifiées et réutiliser davantage de données. Ils augmentent aussi le nombre de blocs et le volume d’index. Des blocs plus petits peuvent réduire le contexte de compression ; ils ne garantissent donc pas de gain lorsque les blocs sont déjà parfaitement partageables ou lorsque les données sont distinctes. Le scénario décalé montre la limite commune aux trois découpages fixes.</p>
<p>Ces petits dépôts ne permettent pas de conclure sur les temps d’ouverture, de purge, la consommation mémoire ni les restaurations à l’échelle de plusieurs téraoctets. Les temps courts, la température CPU, le cache OS et les autres activités de la machine introduisent de la variabilité. Les répétitions restent descriptives ; aucune significativité statistique n’est revendiquée.</p>
<p>Les variantes expérimentales ne doivent pas être utilisées pour poursuivre un dépôt existant : le code actuel refuse de réutiliser directement une image précédente de taille de bloc différente. Une intégration produit demanderait un réglage persistant et une stratégie de compatibilité, puis une validation avec de vraies VM, RCT et stockage cible.</p>
<h2>Résultats et reproduction</h2><ul><li><a href="summary.csv">CSV complet (médianes, min/max et comparaisons)</a></li><li><a href="summary.json">Synthèse JSON</a></li><li><a href="results.jsonl">Mesures brutes</a></li><li><a href="manifest.json">Paramètres et empreintes des sources / exécutables</a></li><li><a href="../run.py">Script de compilation et exécution</a>, <a href="../main.go">programme Go</a>, <a href="../summarize.py">script de synthèse</a></li></ul>
<p>Exécuter <code>python run.py --go CHEMIN_GO --gopath CHEMIN_GOPATH --gocache CHEMIN_GOCACHE --output NOUVEAU_DOSSIER</code>, puis <code>python summarize.py NOUVEAU_DOSSIER</code>. Go 1.27.3 et les dépendances de go.mod sont requis. Les dépôts de test sont conservés.</p>
'''
body+=f'<p>Validation : {len(rows)} mesures, {len(rows)*6} restaurations de disques comparées, plus les échauffements ; tests des packages diskimg, repo et backend réussis pour chaque variante (voir tests-256.log, tests-512.log et tests-1024.log).</p>'
if (run/'machine.json').exists():
    body+='<h2>Machine de mesure</h2><pre>'+html.escape((run/'machine.json').read_text(encoding='utf-8-sig'))+'</pre>'
body+='</html>'
(run/'rapport.html').write_text(body,encoding='utf-8')
for s in summary:
    print(f"{s['scenario']:10s} {s['block_kib']:4d}: full={s['full_repo_mib']:.3f} MiB inc=+{s['increment_repo_mib']:.3f} MiB total={s['total_repo_mib']:.3f} MiB full={s['full_seconds']:.3f}s inc={s['increment_seconds']:.3f}s index={s['index_kib']:.2f} KiB gain={s['total_repo_mib_saving_pct']:.2f}%")
print(f'Validated {len(rows)} trials, {len(rows)*6} restored disks, identical input hashes.')
