set -e
ns=$1
if [[ $ns == "" ]]; then
    ns="openebs"
fi 

find_bd_for_devlink() {
        devlink=$1
        hostname=$2
        bdList=$(kubectl -n $ns get blockdevices -l kubernetes.io/hostname=$hostname -o jsonpath='{.items[*].metadata.name}')
        for bd in $bdList
        do
                links=$(kubectl -n $ns get blockdevices $bd -o jsonpath="{.spec.devlinks[*].links}")
                # remove the list [] from by-id/by-path links output
                links=$(echo $links | tr -d [ | tr -d ])
                if [[ $links == "" ]]; then
                        links=$(kubectl -n $ns get blockdevices $bd -o jsonpath="{.spec.path}")
                fi
                for link in $links
                do
                        if [[ "$devlink" == *"$link"* ]]; then
                                state=$(kubectl -n $ns get blockdevices $bd -o jsonpath="{.status.state}")
                                claimState=$(kubectl -n $ns get blockdevices $bd -o jsonpath="{.status.claimState}")
                                if [[ $state == "Active" && $claimState == "Unclaimed" ]]; then
                                        echo "$bd"
                                        break
                                fi
                        fi
                done
        done
}

cspList=$(kubectl get csp -o jsonpath='{.items[*].metadata.name}')
for csp in $cspList
do
        echo "Verifying blockdevices on $csp"
        pod=$(kubectl -n $ns get pods -l openebs.io/cstor-pool=$csp -o jsonpath="{.items[?(@.status.phase=='Running')].metadata.name}")
        # verify if a running pod for CSP is present or not
        if [[ $pod == "" ]]; then
                echo "No running pod found for CSP $csp in $ns namespace. Please make sure all CSP pods are running state."
                exit 1
        fi
        # verfiy whether CSP and its pod have the same hostname label & nodeSelector respectively
        podHostName=$(kubectl -n $ns get pod $pod -o jsonpath="{.spec.nodeSelector.kubernetes\.io\/hostname}")
        cspHostName=$(kubectl get csp $csp -o jsonpath="{.metadata.labels.kubernetes\.io\/hostname}")
        if [[ $podHostName != $cspHostName ]]; then
                echo "Please update kubernetes.io/hostname label on the CSP $csp with the correct value: $podHostName"
                exit 1
        fi
        devlinks=$(kubectl -n $ns exec -it $pod -c cstor-pool -- zpool status -P | grep \/dev | awk '{print $1}')
        cspBDs=$(kubectl get csp $csp -o jsonpath="{.spec.group[*].blockDevice[*].name}")
        bdIndex="0"
        for bd in $cspBDs
        do
                bdIndex=$(($bdIndex+1))
                oldbd=$bd
                newbd=""
                state=$(kubectl -n $ns get blockdevices $bd -o jsonpath="{.status.state}")
                claimState=$(kubectl -n $ns get blockdevices $bd -o jsonpath="{.status.claimState}")
                # verify whether the BD mentioned in CSP is Active & Claimed
                if [[ $state == "Active" && $claimState == "Claimed" ]]; then
                        # verify whether the node exists for given BD
                        # if yes then it is valid & continue to next BD
                        nodes=$(kubectl get node -l kubernetes.io/hostname=$podHostName --no-headers | wc -l)
                        if [[ $nodes == 1 ]]; then
                                continue
                        fi
                fi
                devIndex="0"
                for devlink in $devlinks
                do
                        devIndex=$(($devIndex+1))
                        if [ $bdIndex == $devIndex ]; then
                                newbd=$(find_bd_for_devlink "$devlink" "$podHostName")
                                if [[ $newbd != "" ]]; then
                                        # if new blockdevice found after reattach deplay the old and new name
                                        echo "Please update $csp blockdevice from $oldbd --> $newbd"
                                else
                                        # if no new blockdevice found for old, put a warning in red
                                        echo "$(tput setaf 1)For $csp inactive blockdevice $oldbd does not have an active blockdevice$(tput sgr0)"
                                fi
                                break
                        fi
                done
        done
done


