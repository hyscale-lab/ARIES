"""Live attribution provenance guards. Legacy offline replay does not use these."""
import hashlib
import json

def region_identity(regions):
    canonical=[]
    for index,region in enumerate(regions):
        base=region["guest_base"];size=region["bytes"];slot=region.get("slot",index)
        if not isinstance(base,int) or not isinstance(size,int) or base<0 or size<=0 or base%4096 or size%4096:
            raise ValueError("invalid authoritative guest RAM region")
        canonical.append({"slot":slot,"guest_base":base,"bytes":size,"page_count":size//4096})
    canonical.sort(key=lambda r:r["guest_base"])
    if any(a["guest_base"]+a["bytes"]>b["guest_base"] for a,b in zip(canonical,canonical[1:])):
        raise ValueError("overlapping guest RAM regions")
    return canonical

def layout_identity(bootstrap):
    kernel=bootstrap["anchors"]["kernel_sha256"]
    config=bootstrap.get("attribution_config",{})
    components={"base":bootstrap["layout"],"owners":config.get("owner_layout_data"),"names":config.get("names_layout_data")}
    for value in components.values():
        if value is not None and value["kernel_sha256"]!=kernel:
            raise ValueError("mixed kernel layout identities")
    raw=json.dumps({"kernel_sha256":kernel,"layouts":components},sort_keys=True,separators=(",",":")).encode()
    return hashlib.sha256(raw).hexdigest()

def bind_bootstrap(bootstrap,generation,regions,epoch):
    bootstrap["capture_binding"]={"vm_generation":generation,"regions":region_identity(regions),
        "bootstrap_epoch":epoch,"layout_identity":layout_identity(bootstrap)}
    return bootstrap

def validate_live_inputs(bootstrap,reading,*,generation,epoch,source_epoch=None,source_record_id=None,
                         consistency="dirty_validated"):
    binding=bootstrap["capture_binding"]
    if not isinstance(generation,str) or not generation or binding["vm_generation"]!=generation or reading.get("vm_generation")!=generation:
        raise ValueError("live footprint/bootstrap VM generation mismatch")
    expected_layout=layout_identity(bootstrap)
    if binding["layout_identity"]!=expected_layout or reading.get("layout_identity")!=expected_layout:
        raise ValueError("live footprint/bootstrap layout identity mismatch")
    if region_identity(reading["regions"])!=region_identity(binding["regions"]):
        raise ValueError("live footprint/bootstrap RAM region mismatch")
    if "capture_epoch" not in reading or reading["capture_epoch"]!=epoch:
        raise ValueError("live footprint expected capture epoch mismatch")
    if consistency=="dirty_validated":
        if not isinstance(epoch,int) or epoch<2 or source_epoch!=epoch-1:
            raise ValueError("source footprint must precede the coordinated capture boundary")
        if not isinstance(source_record_id,str) or not source_record_id:
            raise ValueError("expected immutable footprint record required")
        if reading.get("source_epoch")!=source_epoch or reading.get("source_record_id")!=source_record_id:
            raise ValueError("live footprint source epoch/record mismatch")
    else:
        # Standalone metadata-only consumers must explicitly label manual selection.
        if reading.get("selection_scope")!="explicit_gpas" or epoch is not None:
            raise ValueError("metadata-only manual selection requires explicit_gpas and no epoch")
        selected=reading.get("selected_gpas")
        if not isinstance(selected,list) or len(set(selected))!=len(selected):
            raise ValueError("manual selection requires unique selected_gpas")
        regions=region_identity(binding["regions"])
        if any(not isinstance(g,int) or g<0 or g%4096 or not any(r["guest_base"]<=g<r["guest_base"]+r["bytes"] for r in regions) for g in selected):
            raise ValueError("manual selection outside authoritative RAM")
        if reading.get("accessed_gpas") or reading.get("unknown_gpas") or reading.get("additional_gpas"):
            raise ValueError("manual selection must not claim measured access or dirty evidence")
    return binding
